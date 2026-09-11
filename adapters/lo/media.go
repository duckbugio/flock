package lo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// MaxBytes is the configured cap, so a caller can refuse a known-oversize file before
// any download happens.
func (u *Uploader) MaxBytes() int64 { return u.maxBytes }

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
		return "", err
	}
	return saved, nil
}

// photoFileName names a saved photo. LO photos arrive without a file name, and the file
// path is a reference rather than something with an extension, so the name is generated
// from the message: predictable for the agent, unique per message, and always .jpg —
// which is what the platform stores photos as.
func photoFileName(messageID int64) string {
	return fmt.Sprintf("photo_%d.jpg", messageID)
}
