package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/duckbugio/flock/internal/fsutil"
)

// defaultMaxUploadBytes caps a document/photo download. 20 MiB matches the
// Telegram Bot API getFile download limit; an oversize file is rejected with a
// single friendly notice rather than downloaded.
const defaultMaxUploadBytes int64 = 20 << 20 // 20 MiB

// filePerm is the owner-only permission for bot-created upload files.
const filePerm os.FileMode = 0o600

// ErrUploadTooLarge is returned when the source reports (or the stream exceeds)
// the configured size cap, so the caller can turn it into one friendly notice.
var ErrUploadTooLarge = fsutil.ErrUploadTooLarge

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
// *workspace.Renderer satisfies it via UploadsDir; tests use a fake. The chat id
// is the transport-neutral string id (Telegram's numeric chat id rendered as a
// string), matching the core/workspace.Renderer signature.
type uploadsDirResolver interface {
	UploadsDir(chatID string) (string, error)
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
	uploadsDir, err := u.uploads.UploadsDir(strconv.FormatInt(chatID, 10))
	if err != nil {
		return "", fmt.Errorf("resolve uploads dir: %w", err)
	}

	dest, err := fsutil.DestPath(uploadsDir, fileName, &u.seq)
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
		return "", fmt.Errorf("download file: %w", fsutil.RedactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("download file: status %d", resp.StatusCode)
	}

	saved, err := fsutil.WriteCapped(dest, resp.Body, u.maxBytes, filePerm)
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
