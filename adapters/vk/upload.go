package vk

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
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// defaultMaxUploadBytes caps an inbound attachment download. 20 MiB matches the
// Telegram adapter's cap; an oversize file is rejected with a single friendly
// notice rather than downloaded.
const defaultMaxUploadBytes int64 = 20 << 20 // 20 MiB

// filePerm is the owner-only permission for bot-created upload files.
const filePerm os.FileMode = 0o600

// ErrUploadTooLarge is returned when the stream exceeds the configured size cap,
// so the caller can turn it into one friendly notice.
var ErrUploadTooLarge = errors.New("uploaded file exceeds the size limit")

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

	dest, err := u.destPath(uploadsDir, fileName)
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

	saved, err := u.writeCapped(dest, resp.Body)
	if err != nil {
		_ = os.Remove(dest) // best-effort cleanup of a partial file
		return "", err
	}
	u.logger.Debug("saved upload", "chat_id", chatID, "path", saved)
	return saved, nil
}

// destPath builds the sanitized, collision-safe absolute destination path inside
// uploadsDir and asserts the result is contained within it.
func (u *Uploader) destPath(uploadsDir, fileName string) (string, error) {
	name := sanitizeUploadName(fileName)
	prefix := u.uniquePrefix()
	dest := filepath.Join(uploadsDir, prefix+name)

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
// can never escape the uploads dir. Mirrors the Telegram adapter's sanitizer.
func sanitizeUploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.TrimLeft(name, ".")
	name = strings.TrimSpace(name)
	if name == "" {
		return "upload"
	}
	return name
}
