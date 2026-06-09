// Package fsutil holds the transport-neutral file-download primitives shared by
// the chat adapters' Uploader/VoiceTranscriber: hostile-name sanitizing, a
// collision-safe destination path, a size-capped copy that rejects (rather than
// truncates) oversize streams, and URL-error redaction. The per-adapter URL
// acquisition and download strategy stay in each adapter; only these leaf
// primitives are shared.
package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ErrUploadTooLarge is returned when a stream exceeds the configured size cap, so
// the caller can turn it into one friendly notice.
var ErrUploadTooLarge = errors.New("uploaded file exceeds the size limit")

// SanitizeUploadName reduces a client-supplied file name to a safe basename that
// can never escape the uploads dir: it normalizes Windows-style "\" separators
// (filepath.Base only handles the OS separator), takes path.Base, drops leading
// dots so "..", "..." and dotfiles can't traverse or hide, trims whitespace, and
// falls back to a default when nothing safe remains.
func SanitizeUploadName(name string) string {
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

// UniquePrefix returns a short collision-safe filename prefix (timestamp +
// monotonic counter + random hex) so two uploads with the same sanitized name
// never clash. seq is the caller's per-Uploader counter.
func UniquePrefix(seq *atomic.Uint64) string {
	var rnd [4]byte
	// crypto/rand never returns a short read; on the impossible error path the
	// zero bytes plus the counter still keep the prefix unique.
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("%d-%d-%s-", time.Now().UnixNano(), seq.Add(1), hex.EncodeToString(rnd[:]))
}

// DestPath builds the sanitized, collision-safe absolute destination path inside
// uploadsDir and asserts the result is contained within it, so a hostile fileName
// can never escape the uploads dir.
func DestPath(uploadsDir, fileName string, seq *atomic.Uint64) (string, error) {
	name := SanitizeUploadName(fileName)
	prefix := UniquePrefix(seq)
	dest := filepath.Join(uploadsDir, prefix+name)

	// Defense in depth: assert containment. filepath.Join already cleans the path
	// and the sanitizer strips separators, but verify the result still sits under
	// uploadsDir so a hostile name can never escape.
	rel, err := filepath.Rel(uploadsDir, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refusing upload path outside uploads dir: %q", fileName)
	}
	return dest, nil
}

// WriteCapped streams src into dest under a maxBytes cap (LimitReader). If the
// source is longer than the cap the file is rejected as oversize (ErrUploadTooLarge)
// rather than silently truncated: it reads one extra byte and fails when present.
// dest is created O_EXCL with perm.
func WriteCapped(dest string, src io.Reader, maxBytes int64, perm fs.FileMode) (string, error) {
	//nolint:gosec // G304: dest is a workspace upload path the caller constructs, not raw user input.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return "", fmt.Errorf("create upload file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Read up to maxBytes+1: any byte beyond the cap means the file is oversize.
	limited := io.LimitReader(src, maxBytes+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		return "", fmt.Errorf("write upload file: %w", err)
	}
	if n > maxBytes {
		return "", ErrUploadTooLarge
	}
	return dest, nil
}

// RedactURLError replaces a *url.Error (whose Error() includes the request URL —
// which may carry a bot token or a self-keyed access key) with its underlying
// cause, which carries no URL. Other errors pass through unchanged.
func RedactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
