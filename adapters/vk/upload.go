package vk

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

// defaultMaxUploadBytes caps an inbound attachment download. 20 MiB matches the
// Telegram adapter's cap; an oversize file is rejected with a single friendly
// notice rather than downloaded.
const defaultMaxUploadBytes int64 = 20 << 20 // 20 MiB

// filePerm is the owner-only permission for bot-created upload files.
const filePerm os.FileMode = 0o600

// ErrUploadTooLarge is returned when the stream exceeds the configured size cap,
// so the caller can turn it into one friendly notice.
var ErrUploadTooLarge = fsutil.ErrUploadTooLarge

// uploadsDirResolver resolves (and creates) a chat's uploads directory. The
// *workspace.Renderer satisfies it via UploadsDir; tests use a fake. The chat id
// is the transport-neutral string id (VK peer id rendered as a string).
type uploadsDirResolver interface {
	UploadsDir(chatID string) (string, error)
}

// Uploader downloads a VK inbound attachment by its (self-keyed) URL and saves it
// under a per-chat uploads directory OUTSIDE every repo working tree (a sibling
// of the cloned repos), so user files can never enter a git commit. VK attachment
// URLs carry their own access keys (NOT the community token), so the download is
// a direct GET; the URL is still redacted from any error/log, mirroring the
// Telegram adapter's discipline.
type Uploader struct {
	client   *http.Client
	uploads  uploadsDirResolver
	maxBytes int64
	logger   *slog.Logger
	seq      atomic.Uint64 // collision-safe filename prefix counter
}

// NewUploader builds an Uploader. A nil client defaults to http.DefaultClient, a
// non-positive maxBytes to defaultMaxUploadBytes, and a nil logger to
// slog.Default().
func NewUploader(client *http.Client, uploads uploadsDirResolver, maxBytes int64, logger *slog.Logger) *Uploader {
	if client == nil {
		client = http.DefaultClient
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxUploadBytes
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Uploader{client: client, uploads: uploads, maxBytes: maxBytes, logger: logger}
}

// MaxBytes is the configured download cap.
func (u *Uploader) MaxBytes() int64 { return u.maxBytes }

// Save downloads url's bytes (capped at maxBytes) and writes them to a sanitized,
// collision-safe path under chatID's uploads directory, returning the saved
// ABSOLUTE path. fileName is the client-supplied name and is treated as hostile:
// only its sanitized base is used so the saved file can never escape the uploads
// dir. An oversize or undownloadable file returns an error (ErrUploadTooLarge for
// the size case) the caller turns into one friendly notice; the error never
// carries the attachment URL.
func (u *Uploader) Save(ctx context.Context, chatID int64, url, fileName string) (string, error) {
	uploadsDir, err := u.uploads.UploadsDir(strconv.FormatInt(chatID, 10))
	if err != nil {
		return "", fmt.Errorf("resolve uploads dir: %w", err)
	}

	dest, err := fsutil.DestPath(uploadsDir, fileName, &u.seq)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// The url carries the attachment's access key — never include it in the error.
		return "", fmt.Errorf("build download request: %w", redactURLError(err))
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download attachment: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("download attachment: status %d", resp.StatusCode)
	}

	saved, err := fsutil.WriteCapped(dest, resp.Body, u.maxBytes, filePerm)
	if err != nil {
		_ = os.Remove(dest) // best-effort cleanup of a partial file
		return "", err
	}
	u.logger.Debug("saved upload", "chat_id", chatID, "path", saved)
	return saved, nil
}
