package pending_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/duckbugio/flock/core/pending"
)

// TestSetGetAllRoundTrip asserts a marker set on one store is reloaded by a fresh
// store opened on the same path (the restart path) and surfaced by All().
func TestSetGetAllRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := pending.Marker{Prompt: "do the thing", AnchorMsgID: "42", StartedAt: 1234}
	if err := s.Set("100", want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Reopen on the same path: the marker must survive (this is what makes a
	// killed run resumable after a restart).
	s2, err := pending.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	all := s2.All()
	got, ok := all["100"]
	if !ok {
		t.Fatalf("marker for chat 100 missing after reopen; all=%v", all)
	}
	if got != want {
		t.Fatalf("reloaded marker = %+v, want %+v", got, want)
	}
}

// TestSetIsAtomicRewrite asserts Set rewrites the whole file atomically: a second
// Set for a different chat does not lose the first, and no temp files linger.
func TestSetIsAtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("1", pending.Marker{Prompt: "a"}); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	if err := s.Set("2", pending.Marker{Prompt: "b"}); err != nil {
		t.Fatalf("Set 2: %v", err)
	}

	all := s.All()
	if len(all) != 2 || all["1"].Prompt != "a" || all["2"].Prompt != "b" {
		t.Fatalf("after two Sets, all=%v want both markers", all)
	}

	// The atomic write removes its temp file after the rename — only the final
	// store file should remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "pending.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir entries = %v, want only pending.json (no leftover temp files)", names)
	}
}

// TestDelete asserts a deleted marker is gone after Delete and that deleting an
// absent chat is a harmless no-op.
func TestDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("100", pending.Marker{Prompt: "x"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete("100"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.All()["100"]; ok {
		t.Fatal("marker still present after Delete")
	}
	// Deleting an absent chat must not error.
	if err := s.Delete("999"); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
}

// TestNilStoreSafe asserts a nil *FileStore behaves as a no-op store so callers
// can keep running when the store fails to open (best-effort startup wiring).
func TestNilStoreSafe(t *testing.T) {
	var s *pending.FileStore
	if err := s.Set("1", pending.Marker{}); err != nil {
		t.Fatalf("nil Set: %v", err)
	}
	if err := s.Delete("1"); err != nil {
		t.Fatalf("nil Delete: %v", err)
	}
	if got := s.All(); len(got) != 0 {
		t.Fatalf("nil All = %v, want empty", got)
	}
}
