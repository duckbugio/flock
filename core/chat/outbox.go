package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// defaultMaxOutboxBytes caps a single outbox file's size. 20 MiB matches the
// inbound upload cap; an oversize file is skipped with a logged warning rather
// than sent.
const defaultMaxOutboxBytes int64 = 20 << 20 // 20 MiB

// defaultMaxOutboxFiles bounds how many files a single sweep delivers, so a run
// that fills the outbox can't spam the chat. Files beyond the cap are left in
// place for a future sweep.
const defaultMaxOutboxFiles = 10

// sentSubdir is the archive directory under a chat's outbox. A successfully sent
// file is moved here (os.Rename) rather than deleted, and the sweep never
// recurses into it, so it is naturally skipped and never re-swept (AC3).
const sentSubdir = "sent"

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// outboxDirResolver resolves (and creates) a chat's outbox directory. The
// *workspace.Renderer satisfies it via OutboxDir; tests use a fake. This mirrors
// uploadsDirResolver.
type outboxDirResolver interface {
	OutboxDir(chatID ChatID) (string, error)
}

// documentSender is the subset of the Transport the Sweeper needs: it uploads
// one file to a chat as a document. The Transport (and the test fakes) satisfy it.
type documentSender interface {
	SendDocument(ctx context.Context, chatID ChatID, name string, data io.Reader) error
}

// Sweeper delivers files a run left in a chat's outbox directory as Telegram
// documents, then archives each sent file under outbox/sent/. The outbox lives
// OUTSIDE every repo working tree (a sibling of the cloned repos), so the files
// it sweeps can never be ones inside a git tree. It reads LOCAL files only (no
// HTTP download), so unlike Uploader it needs no token-bearing download seam.
type Sweeper struct {
	outbox   outboxDirResolver
	maxBytes int64
	maxFiles int
	logger   *slog.Logger
}

// NewSweeper builds a Sweeper. A non-positive maxBytes falls back to
// defaultMaxOutboxBytes and a non-positive maxFiles to defaultMaxOutboxFiles
// (mirroring the uploader's "non-positive → built-in default"). A nil logger
// defaults to slog.Default().
func NewSweeper(outbox outboxDirResolver, maxBytes int64, maxFiles int, logger *slog.Logger) *Sweeper {
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutboxBytes
	}
	if maxFiles <= 0 {
		maxFiles = defaultMaxOutboxFiles
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sweeper{
		outbox:   outbox,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
		logger:   logger,
	}
}

// Sweep resolves chatID's outbox directory and delivers every regular file
// DIRECTLY inside it (no recursion into subdirs, so outbox/sent/ is skipped) as a
// Telegram document via c, archiving each successfully sent file under
// outbox/sent/. It is best-effort and per-file isolated: an oversize, unreadable,
// or send-failed file is logged and skipped without blocking the others, and a
// send failure leaves the file in place (NOT archived) so a future sweep can
// retry. An empty or absent outbox is a no-op. At most maxFiles files are
// delivered per sweep.
func (s *Sweeper) Sweep(ctx context.Context, chatID ChatID, c documentSender) {
	outboxDir, err := s.outbox.OutboxDir(chatID)
	if err != nil {
		s.logger.Warn("resolve outbox dir", "chat_id", chatID, "error", err)
		return
	}

	entries, err := os.ReadDir(outboxDir)
	if err != nil {
		// An absent outbox is a no-op (OutboxDir creates it, so this is defensive);
		// any other read error is logged and skipped.
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("read outbox dir", "chat_id", chatID, "error", err)
		}
		return
	}

	sent := 0
	for _, e := range entries {
		if sent >= s.maxFiles {
			s.logger.Warn("outbox file-count cap reached; remaining files deferred",
				"chat_id", chatID, "max_files", s.maxFiles)
			break
		}
		if ctx.Err() != nil {
			return
		}
		if s.deliver(ctx, chatID, outboxDir, e.Name(), c) {
			sent++
		}
	}
}

// deliver sends one named outbox entry as a document and archives it on success.
// It returns true only when the file was actually sent (so the count cap advances
// solely on real deliveries). It is defensive on every step: a non-regular entry
// (dir, symlink), an oversize file, an unreadable file, or a send failure is
// logged and skipped, never panicking or aborting the sweep.
func (s *Sweeper) deliver(ctx context.Context, chatID ChatID, outboxDir, name string, c documentSender) bool {
	path := filepath.Join(outboxDir, name)

	// Defense in depth: assert the entry stays directly under outboxDir. ReadDir
	// already yields base names, but verify containment like upload.go's destPath
	// so a surprising name can never escape (AC3).
	if !contained(outboxDir, path) {
		s.logger.Warn("refusing outbox path outside outbox dir", "chat_id", chatID, "name", name)
		return false
	}

	// Lstat (NOT Stat) so a symlink is seen as a symlink and skipped, never
	// followed — a symlink pointing outside the outbox must never be sent (AC3).
	info, err := os.Lstat(path)
	if err != nil {
		s.logger.Warn("stat outbox file", "chat_id", chatID, "name", name, "error", err)
		return false
	}
	// Only REGULAR files directly inside outbox/ are sent: this skips the sent/
	// archive dir (no recursion), any other subdirectory, and symlinks.
	if !info.Mode().IsRegular() {
		return false
	}
	if info.Size() > s.maxBytes {
		s.logger.Warn("skipping oversize outbox file",
			"chat_id", chatID, "name", name, "size", info.Size(), "max_bytes", s.maxBytes)
		return false
	}

	f, err := os.Open(path) //nolint:gosec // path is contained directly under the chat's outbox dir
	if err != nil {
		s.logger.Warn("open outbox file", "chat_id", chatID, "name", name, "error", err)
		return false
	}
	sendErr := c.SendDocument(ctx, chatID, name, f)
	_ = f.Close()
	if sendErr != nil {
		// Leave the file in outbox/ (do NOT archive) so a future sweep retries; the
		// file-count cap bounds any spam (AC4).
		s.logger.Warn("send outbox document", "chat_id", chatID, "name", name, "error", sendErr)
		return false
	}

	if err := s.archive(outboxDir, name); err != nil {
		// The file WAS delivered; a failed archive only risks a duplicate next sweep.
		// Log and move on rather than re-sending in the same sweep.
		s.logger.Warn("archive sent outbox file", "chat_id", chatID, "name", name, "error", err)
	}
	s.logger.Debug("sent outbox document", "chat_id", chatID, "name", name)
	return true
}

// archive moves a successfully sent file into the outbox/sent/ subdirectory
// (created on first use), preserving it rather than deleting it (AC1/AC5).
func (s *Sweeper) archive(outboxDir, name string) error {
	sentDir := filepath.Join(outboxDir, sentSubdir)
	if err := os.MkdirAll(sentDir, dirPerm); err != nil {
		return fmt.Errorf("create sent dir: %w", err)
	}
	if err := os.Rename(filepath.Join(outboxDir, name), filepath.Join(sentDir, name)); err != nil {
		return fmt.Errorf("rename into sent dir: %w", err)
	}
	return nil
}

// contained reports whether child resolves to a path directly within (or equal
// to a child of) base, using filepath.Rel like upload.go's containment assert.
func contained(base, child string) bool {
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
