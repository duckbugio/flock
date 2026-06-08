package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// recordingSender is a documentSender that records every sent filename and can be
// configured to fail for a specific name (to exercise the send-failure path).
type recordingSender struct {
	mu       sync.Mutex
	names    []string
	failName string // SendDocument returns an error for this name
}

func (s *recordingSender) SendDocument(_ context.Context, _ int64, name string, data io.Reader) error {
	// Drain the reader like a real upload would, so the source file is read before
	// the sweeper closes it.
	_, _ = io.Copy(io.Discard, data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == s.failName {
		return errors.New("send failed")
	}
	s.names = append(s.names, name)
	return nil
}

func (s *recordingSender) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.names...)
	sort.Strings(out)
	return out
}

// fakeOutbox resolves every chat to one fixed directory.
type fakeOutbox struct{ dir string }

func (f fakeOutbox) OutboxDir(int64) (string, error) { return f.dir, nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestSweeper(dir string, maxBytes int64, maxFiles int) *Sweeper {
	return NewSweeper(fakeOutbox{dir: dir}, maxBytes, maxFiles, discardLogger())
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestSweepSendsAndArchives covers AC1: a regular file in outbox/ is sent as a
// document and then MOVED to outbox/sent/ (not deleted).
func TestSweepSendsAndArchives(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "report.txt", []byte("hello"))

	sender := &recordingSender{}
	newTestSweeper(dir, 0, 0).Sweep(context.Background(), 7, sender)

	if got := sender.sent(); len(got) != 1 || got[0] != "report.txt" {
		t.Fatalf("sent = %v, want [report.txt]", got)
	}
	// Archived, not deleted: gone from outbox/, present in outbox/sent/.
	if _, err := os.Stat(filepath.Join(dir, "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("file still in outbox after send (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", "report.txt")); err != nil {
		t.Fatalf("file not archived under sent/: %v", err)
	}
}

// TestSweepSkipsSubdirsAndSymlinks covers AC3: a subdirectory is not recursed
// into, and a symlink (even one pointing outside the outbox) is skipped, not
// followed. Only the regular file is sent.
func TestSweepSkipsSubdirsAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.txt", []byte("data"))

	// A subdirectory with a file inside: must NOT be recursed into.
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	writeFile(t, sub, "deep.txt", []byte("deep"))

	// A pre-existing sent/ archive dir with a file: must be skipped (never
	// re-swept).
	sentDir := filepath.Join(dir, "sent")
	if err := os.MkdirAll(sentDir, 0o755); err != nil {
		t.Fatalf("mkdir sent: %v", err)
	}
	writeFile(t, sentDir, "old.txt", []byte("old"))

	// A symlink pointing OUTSIDE the outbox (to a secret file): must never be
	// followed/sent.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	sender := &recordingSender{}
	newTestSweeper(dir, 0, 0).Sweep(context.Background(), 7, sender)

	if got := sender.sent(); len(got) != 1 || got[0] != "ok.txt" {
		t.Fatalf("sent = %v, want only [ok.txt] (subdir/sent/symlink skipped)", got)
	}
	// The symlink target must remain untouched in place (never archived/moved).
	if _, err := os.Stat(filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink should remain in outbox: %v", err)
	}
}

// TestSweepOversizeSkippedSiblingSent covers AC4: an oversize file is skipped but
// a normal sibling is still delivered.
func TestSweepOversizeSkippedSiblingSent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.bin", make([]byte, 100))
	writeFile(t, dir, "small.txt", []byte("ok"))

	sender := &recordingSender{}
	// maxBytes = 10: big.bin (100B) is over, small.txt (2B) is under.
	newTestSweeper(dir, 10, 0).Sweep(context.Background(), 7, sender)

	if got := sender.sent(); len(got) != 1 || got[0] != "small.txt" {
		t.Fatalf("sent = %v, want only [small.txt] (oversize skipped)", got)
	}
	// Oversize file stays in place (not archived).
	if _, err := os.Stat(filepath.Join(dir, "big.bin")); err != nil {
		t.Fatalf("oversize file should remain in outbox: %v", err)
	}
}

// TestSweepFileCountCap covers AC4: at most maxFiles files are delivered per
// sweep; the remainder stay in place for a future sweep.
func TestSweepFileCountCap(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, dir, n, []byte("x"))
	}

	sender := &recordingSender{}
	newTestSweeper(dir, 0, 2).Sweep(context.Background(), 7, sender)

	if got := sender.sent(); len(got) != 2 {
		t.Fatalf("sent %d files, want 2 (count cap)", len(got))
	}
	// Exactly one file remains in outbox/ (the deferred one), and two are archived.
	remaining := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Type().IsRegular() {
			remaining++
		}
	}
	if remaining != 1 {
		t.Fatalf("remaining regular files = %d, want 1 (deferred by cap)", remaining)
	}
}

// TestSweepSendFailureLeavesFile covers AC4: a send failure leaves the file in
// outbox/ (NOT archived) so a future sweep can retry, while siblings still send.
func TestSweepSendFailureLeavesFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "fail.txt", []byte("x"))
	writeFile(t, dir, "ok.txt", []byte("y"))

	sender := &recordingSender{failName: "fail.txt"}
	newTestSweeper(dir, 0, 0).Sweep(context.Background(), 7, sender)

	if got := sender.sent(); len(got) != 1 || got[0] != "ok.txt" {
		t.Fatalf("sent = %v, want [ok.txt] (failed sibling does not block)", got)
	}
	// The failed file stays in outbox/ for retry; it is NOT in sent/.
	if _, err := os.Stat(filepath.Join(dir, "fail.txt")); err != nil {
		t.Fatalf("failed file should remain in outbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", "fail.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed file must NOT be archived (err=%v)", err)
	}
	// The good file IS archived.
	if _, err := os.Stat(filepath.Join(dir, "sent", "ok.txt")); err != nil {
		t.Fatalf("ok.txt not archived: %v", err)
	}
}

// TestSweepEmptyAndAbsentNoOp covers AC2's no-op clause: an empty outbox (and an
// absent one) deliver nothing and error nothing.
func TestSweepEmptyAndAbsentNoOp(t *testing.T) {
	// Empty existing dir.
	empty := t.TempDir()
	sender := &recordingSender{}
	newTestSweeper(empty, 0, 0).Sweep(context.Background(), 7, sender)
	if got := sender.sent(); len(got) != 0 {
		t.Fatalf("empty outbox sent %v, want none", got)
	}

	// Absent dir: OutboxDir would normally create it, but assert ReadDir on a
	// missing path is a clean no-op too (resolver returns a non-existent path).
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	sender2 := &recordingSender{}
	newTestSweeper(missing, 0, 0).Sweep(context.Background(), 7, sender2)
	if got := sender2.sent(); len(got) != 0 {
		t.Fatalf("absent outbox sent %v, want none", got)
	}
}
