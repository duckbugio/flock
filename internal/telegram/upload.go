package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// defaultMaxUploadBytes caps a document/photo download. 20 MiB matches the
// Telegram Bot API getFile download limit; an oversize file is rejected with a
// single friendly notice rather than downloaded.
const defaultMaxUploadBytes int64 = 20 << 20 // 20 MiB

// ErrUploadTooLarge is returned when the source reports (or the stream exceeds)
// the configured size cap, so the caller can turn it into one friendly notice.
var ErrUploadTooLarge = errors.New("uploaded file exceeds the size limit")

// Uploader downloads a Telegram document/photo by file_id and saves it under a
// per-chat uploads directory that lives OUTSIDE every repo working tree (a
// sibling of the cloned repos), so user files can never enter a git commit. It
// reuses the voice download seam (fileSource / redactURLError / io.LimitReader)
// so the token-bearing download URL is never logged or surfaced.
type Uploader struct {
	source   fileSource
	client   *http.Client
	uploads  uploadsDirResolver
	maxBytes int64
	logger   *slog.Logger
	seq      atomic.Uint64 // collision-safe filename prefix counter
}

// uploadsDirResolver resolves (and creates) a chat's uploads directory. The
// *workspace.Renderer satisfies it via UploadsDir; tests use a fake.
type uploadsDirResolver interface {
	UploadsDir(chatID int64) (string, error)
}

// NewUploader builds an Uploader. A nil client defaults to http.DefaultClient, a
// non-positive maxBytes to defaultMaxUploadBytes, and a nil logger to
// slog.Default().
func NewUploader(
	source fileSource, client *http.Client, uploads uploadsDirResolver, maxBytes int64, logger *slog.Logger,
) *Uploader {
	if client == nil {
		client = http.DefaultClient
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxUploadBytes
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Uploader{
		source:   source,
		client:   client,
		uploads:  uploads,
		maxBytes: maxBytes,
		logger:   logger,
	}
}

// MaxBytes is the configured download cap. Callers may pre-check a known file
// size against it to reject an oversize upload before any download.
func (u *Uploader) MaxBytes() int64 { return u.maxBytes }

// Save downloads fileID's bytes (capped at maxBytes) and writes them to a
// sanitized, collision-safe path under chatID's uploads directory, returning the
// saved ABSOLUTE path. fileName is the client-supplied name and is treated as
// hostile: only its base is used and path separators / leading dots are stripped,
// so the saved file can never escape the uploads dir. An oversize or
// undownloadable file returns an error (ErrUploadTooLarge for the size case) the
// caller turns into one friendly notice; the error never carries the bot token.
func (u *Uploader) Save(ctx context.Context, chatID int64, fileID, fileName string) (string, error) {
	uploadsDir, err := u.uploads.UploadsDir(chatID)
	if err != nil {
		return "", fmt.Errorf("resolve uploads dir: %w", err)
	}

	dest, err := u.destPath(uploadsDir, fileName)
	if err != nil {
		return "", err
	}

	filePath, err := u.source.FileInfo(ctx, fileID)
	if err != nil {
		return "", err
	}

	url := u.source.DownloadURL(filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// url embeds the token — never include it in the error.
		return "", fmt.Errorf("build download request: %w", err)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		// A transport error from client.Do is a *url.Error whose message embeds the
		// request URL (and thus the bot token); strip it so nothing leaks to a log
		// or a user-facing error.
		return "", fmt.Errorf("download file: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("download file: status %d", resp.StatusCode)
	}

	saved, err := u.writeCapped(dest, resp.Body)
	if err != nil {
		// Best-effort cleanup of a partial file; ignore its error.
		_ = os.Remove(dest)
		return "", err
	}
	// Debug-only: never log the token-bearing download URL — only the chat and the
	// saved sibling-of-repos path (safe to surface).
	u.logger.Debug("saved upload", "chat_id", chatID, "path", saved)
	return saved, nil
}

// destPath builds the sanitized, collision-safe absolute destination path inside
// uploadsDir and asserts the result is contained within it.
func (u *Uploader) destPath(uploadsDir, fileName string) (string, error) {
	name := sanitizeUploadName(fileName)
	prefix := u.uniquePrefix()
	dest := filepath.Join(uploadsDir, prefix+name)

	// Defense in depth: assert containment. filepath.Join already cleans the path
	// and the sanitizer strips separators, but verify the result still sits under
	// uploadsDir so a hostile name can never escape (AC3).
	rel, err := filepath.Rel(uploadsDir, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refusing upload path outside uploads dir: %q", fileName)
	}
	return dest, nil
}

// uniquePrefix returns a short collision-safe prefix (timestamp + monotonic
// counter + random hex) so two uploads with the same sanitized name never clash.
func (u *Uploader) uniquePrefix() string {
	var rnd [4]byte
	// crypto/rand never returns a short read; on the impossible error path the
	// zero bytes plus the counter still keep the prefix unique.
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("%d-%d-%s-", time.Now().UnixNano(), u.seq.Add(1), hex.EncodeToString(rnd[:]))
}

// writeCapped streams src into dest under a maxBytes cap (LimitReader). If the
// source is longer than the cap the file is rejected as oversize rather than
// silently truncated: we read one extra byte and fail when it is present.
func (u *Uploader) writeCapped(dest string, src io.Reader) (string, error) {
	//nolint:gosec // G304: dest is a workspace upload path we construct, not raw user input.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", fmt.Errorf("create upload file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Read up to maxBytes+1: any byte beyond the cap means the file is oversize.
	limited := io.LimitReader(src, u.maxBytes+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		return "", fmt.Errorf("write upload file: %w", err)
	}
	if n > u.maxBytes {
		return "", ErrUploadTooLarge
	}
	return dest, nil
}

// sanitizeUploadName reduces a client-supplied file name to a safe basename that
// can never escape the uploads dir: it takes filepath.Base, then also strips any
// Windows-style "\" segments (filepath.Base only handles the OS separator),
// drops leading dots so "..", "..." and dotfiles can't traverse or hide, and
// falls back to a default when nothing safe remains.
func sanitizeUploadName(name string) string {
	// Normalize Windows separators so "..\..\x" collapses too, then take the base.
	// path.Base operates on slash-separated paths regardless of host OS, so it
	// reduces the normalized name to its last element ("." for an empty/all-slash
	// input). filepath.Base alone would miss "\" segments on Linux.
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	// Strip every leading dot so ".", "..", "...", ".env" cannot traverse or hide.
	name = strings.TrimLeft(name, ".")
	name = strings.TrimSpace(name)
	if name == "" {
		return "upload"
	}
	return name
}
