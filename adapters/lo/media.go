package lo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/duckbugio/flock/internal/fsutil"
)

// defaultMaxUploadBytes caps an inbound download. 20 MiB matches the Telegram adapter's
// cap, which is the size users already expect a bot to accept; an oversize file is
// refused with one notice instead of being streamed to disk.
const defaultMaxUploadBytes int64 = 20 << 20

// filePerm keeps a saved upload readable only by the bot process.
const filePerm os.FileMode = 0o600

// ErrNoBytes reports a reference this platform will not serve bytes for. LO answers
// getFile for audio and video WITHOUT a file_path on purpose — the reference is usable
// for an echo, the bytes have no address. It is a normal outcome, not a fault, and the
// caller turns it into a specific sentence rather than a generic failure.
var ErrNoBytes = errors.New("lo: this platform serves no bytes for that file")

// ErrUploadTooLarge is re-exported so callers match one error for the size case.
var ErrUploadTooLarge = fsutil.ErrUploadTooLarge

// fileSource is the Client seam the Uploader needs: resolve a reference, then stream it.
// Splitting it from *Client keeps the download path testable without an HTTP server that
// has to speak the whole Bot API envelope.
type fileSource interface {
	GetFile(ctx context.Context, fileID string) (File, error)
	Download(ctx context.Context, filePath string) (io.ReadCloser, error)
}

// uploadsDirResolver resolves (and creates) a chat's uploads directory —
// *workspace.Renderer satisfies it via UploadsDir. The directory is a SIBLING of the
// cloned repositories, so a user's file can never be swept into a commit.
type uploadsDirResolver interface {
	UploadsDir(chatID string) (string, error)
}

// Uploader saves an inbound LO file into the per-chat uploads directory and returns the
// absolute path, which is what the agent is told to open. It never returns the download
// URL to a caller: that URL carries the bot token.
type Uploader struct {
	source   fileSource
	uploads  uploadsDirResolver
	maxBytes int64
	logger   *slog.Logger
	seq      atomic.Uint64
}

// NewUploader builds an Uploader. A non-positive maxBytes selects the default cap and a
// nil logger the default logger.
func NewUploader(source fileSource, uploads uploadsDirResolver, maxBytes int64, logger *slog.Logger) *Uploader {
	if maxBytes <= 0 {
		maxBytes = defaultMaxUploadBytes
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Uploader{source: source, uploads: uploads, maxBytes: maxBytes, logger: logger}
}

// Save downloads fileID and writes it under chatID's uploads directory, returning the
// saved absolute path.
//
// fileName is client-supplied and treated as hostile: fsutil keeps only a sanitized base
// with a unique prefix, so a crafted name cannot escape the uploads directory. An empty
// name (LO photos carry none) falls back to a generated one.
func (u *Uploader) Save(ctx context.Context, chatID, fileID, fileName string) (string, error) {
	uploadsDir, err := u.uploads.UploadsDir(chatID)
	if err != nil {
		return "", fmt.Errorf("resolve uploads dir: %w", err)
	}
	dest, err := fsutil.DestPath(uploadsDir, fileName, &u.seq)
	if err != nil {
		return "", err
	}

	file, err := u.source.GetFile(ctx, fileID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(file.FilePath) == "" {
		return "", ErrNoBytes
	}
	// LO omits file_size for files it has not measured, so a zero is "unknown", never
	// "empty" — only a positive value can refuse a download before it starts.
	if file.FileSize > 0 && file.FileSize > u.maxBytes {
		return "", ErrUploadTooLarge
	}

	body, err := u.source.Download(ctx, file.FilePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()

	saved, err := fsutil.WriteCapped(dest, body, u.maxBytes, filePerm)
	if err != nil {
		// Best-effort cleanup of the partial file, as the Telegram and VK uploaders do. A
		// file left behind after a refused upload is worse than a missing one: it keeps the
		// bytes of something the user was told did not arrive, and nothing ever deletes it.
		//
		// A cleanup that ITSELF fails is logged rather than returned: the caller is already
		// being told the upload failed, and a second error would replace the reason the user
		// needs with one only an operator can act on. The operator still needs it, though —
		// this is the one path that leaves bytes on disk nobody will collect.
		if rmErr := os.Remove(dest); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			u.logger.Warn("lo: could not remove a partial upload", "error", rmErr)
		}
		return "", err
	}
	return saved, nil
}

// documentExtensions maps a declared type to the extension a saved document gets.
//
// An explicit table rather than mime.ExtensionsByType, and not for determinism alone. Go mixes
// the host's /etc/mime.types into that table, so the answer depends on the container: text/plain
// comes back as "txt text pot brf srt" and image/jpeg as "jpeg jpg jpe jfif". Picking the first
// alphabetically — which an earlier version did, for stability — chose ".brf" for a text file
// and ".jfif" for a photo, both worse than any of the obvious answers. These are the types a
// chat actually carries; everything else is bytes nobody described, and ".bin" says so.
//
//nolint:gochecknoglobals // A fixed table, read-only, kept beside the function that uses it.
var documentExtensions = map[string]string{
	"application/pdf":  ".pdf",
	"application/json": ".json",
	"application/zip":  ".zip",
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"text/html":        ".html",
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
}

// documentFileName names a saved document when the platform sent no file name. LO fills
// file_name only "usually", and the fallback in fsutil is the bare word "upload" — an agent
// handed a path with no extension is looking at exactly the opaque id this adapter's inbound
// path exists to avoid. The declared MIME type is the only other thing the message carries.
func documentFileName(messageID int64, mimeType string) string {
	ext := ".bin"
	if media, _, err := mime.ParseMediaType(mimeType); err == nil {
		if known, ok := documentExtensions[strings.ToLower(media)]; ok {
			ext = known
		}
	}
	return fmt.Sprintf("document_%d%s", messageID, ext)
}

// photoFileName names a saved photo. LO photos arrive without a file name, and the file path
// is a reference rather than something with an extension, so the name is generated from the
// message: predictable for the agent and unique per message.

// photoFileName names a saved photo BEFORE its bytes have been seen. LO photos arrive without a
// file name, and the file path is a reference rather than something with an extension, so the
// name is generated from the message: predictable for the agent and unique per message.//
// The extension here is a guess. It is corrected the moment the bytes are on disk — see
// photoExtension — because something DOES read a media type out of this name: core/chat derives
// the vision block's type from the saved path, so a PNG named .jpg reaches the model declared as
// a JPEG.
func photoFileName(messageID int64) string {
	return fmt.Sprintf("photo_%d.jpg", messageID)
}

// photoExtensions maps what http.DetectContentType reports to the extension core/chat reads a
// media type back out of. The four entries are exactly the formats the vision block can carry
// — core/chat's photoMediaType knows no others and answers image/jpeg for anything else — so a
// picture in a format outside this table cannot be shown to the model under a true type, which
// is why the caller refuses one rather than renaming it.
//
//nolint:gochecknoglobals // A fixed table, read-only, beside the function that uses it.
var photoExtensions = map[string]string{
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
	"image/jpeg": ".jpg",
}

// emptyFileType is what nameByContent answers for a file with no bytes at all. It is not a
// media type the sniffer can produce — DetectContentType calls an empty body text/plain — and
// it exists so the caller can tell "nothing arrived" from "something the sniffer did not
// recognise", which are opposite decisions.
const emptyFileType = "application/x-empty"

// nameByContent renames a saved photo to the extension its BYTES say it has, and answers the
// path it now lives at.
//
// The name is not decoration. core/chat builds the model's vision block from the saved path,
// so a PNG saved as .jpg is DECLARED to the model as a JPEG — a claim about the bytes made by
// a file name this adapter invented. Sniffing costs one read of the first 512 bytes, which is
// what http.DetectContentType looks at.
//
// It answers the detected type as well, because the caller has a decision to make with it that
// a name cannot carry: bytes that are NOT an image must not be handed to the model as one. A
// storage error page served with 200, or an empty body, would otherwise reach the model
// declared as a JPEG — the same lie about the bytes this function exists to stop, one step
// later.
//
// A failure to sniff keeps the original path and answers an empty type, which the caller reads
// as "unknown, not disproven": the file is saved and openable either way, and losing a correct
// media type is a degradation while losing the file is not.
func nameByContent(path string, logger *slog.Logger) (saved, detected string) {
	file, err := os.Open(path) //nolint:gosec // Path is one this adapter just wrote.
	if err != nil {
		logger.Warn("lo: could not sniff a saved photo", "error", err)
		return path, ""
	}
	// sniffBytes is what http.DetectContentType reads, and reading more would be wasted.
	const sniffBytes = 512
	head := make([]byte, sniffBytes)
	// ReadFull rather than Read: a short read is legal for an io.Reader, and the answer here is
	// no longer only a file name — it decides whether the picture is shown at all.
	n, err := io.ReadFull(file, head)
	_ = file.Close()
	switch {
	case n == 0 && (err == nil || errors.Is(err, io.EOF)):
		// An empty file. Zero bytes are not a picture, and the caller must hear that rather
		// than a shrug.
		return path, emptyFileType
	case n == 0:
		logger.Warn("lo: could not read a saved photo", "error", err)
		return path, ""
	}
	// DetectContentType may append parameters ("text/plain; charset=utf-8"); only the type
	// itself names a format.
	mediaType, _, _ := strings.Cut(http.DetectContentType(head[:n]), ";")
	mediaType = strings.TrimSpace(mediaType)
	ext, ok := photoExtensions[mediaType]
	if !ok || strings.HasSuffix(path, ext) {
		return path, mediaType
	}
	renamed := strings.TrimSuffix(path, filepath.Ext(path)) + ext
	if err := os.Rename(path, renamed); err != nil {
		logger.Warn("lo: could not rename a saved photo to its real type", "error", err)
		return path, mediaType
	}
	return renamed, mediaType
}
