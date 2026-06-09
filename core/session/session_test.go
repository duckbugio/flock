//nolint:testpackage // intentionally whitebox to test unexported session store internals
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestSetGet asserts a stored id round-trips and an unknown chat reports ok=false (AC1).
func TestSetGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, ok := s.Get(42); ok {
		t.Fatal("Get of an unknown chat returned ok=true")
	}

	if err := s.Set(42, "sess-abc"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := s.Get(42)
	if !ok || got != "sess-abc" {
		t.Fatalf("Get = (%q, %v), want (%q, true)", got, ok, "sess-abc")
	}
}

// TestSetEmptyIsNoop asserts an empty session id is ignored, so a failed run
// never blanks a good stored id.
func TestSetEmptyIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set(7, "good"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(7, ""); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	got, ok := s.Get(7)
	if !ok || got != "good" {
		t.Fatalf("empty Set overwrote stored id: got (%q, %v)", got, ok)
	}
}

// TestPersistenceAcrossReopen simulates a process restart: a fresh Open of the
// SAME path returns the previously stored ids (AC1).
func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s1.Set(100, "sess-100"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s1.Set(200, "sess-200"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Reopen the same path — this is the restart.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for id, want := range map[int64]string{100: "sess-100", 200: "sess-200"} {
		got, ok := s2.Get(id)
		if !ok || got != want {
			t.Fatalf("after reopen Get(%d) = (%q, %v), want (%q, true)", id, got, ok, want)
		}
	}
}

// TestDelete asserts Delete removes an id and persists the removal.
func TestDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set(1, "x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete(1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("Get after Delete returned ok=true")
	}

	// The removal must survive a reopen.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := s2.Get(1); ok {
		t.Fatal("deleted id reappeared after reopen")
	}
}

// TestAtomicWriteNoCorruption asserts the file is always valid JSON after many
// updates (the temp+rename write never leaves a corrupt store).
func TestAtomicWriteNoCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 50; i++ {
		if err := s.Set(int64(i), fmt.Sprintf("sess-%d", i)); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	// A fresh Open re-parses the file; a corrupt one would error here.
	if _, err := Open(path); err != nil {
		t.Fatalf("store corrupted after many writes: %v", err)
	}
	// No stray temp files left behind in the directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "sessions.json" {
			t.Fatalf("unexpected leftover file in store dir: %s", e.Name())
		}
	}
}

// TestMissingFileIsEmptyStore asserts Open of a non-existent path yields an
// empty (usable) store rather than an error.
func TestMissingFileIsEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "sessions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open of missing file: %v", err)
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("fresh store reported a stored id")
	}
	if err := s.Set(1, "x"); err != nil {
		t.Fatalf("Set on fresh store: %v", err)
	}
}

// TestConcurrentSetGetRace hammers Set/Get from many goroutines so `-race`
// catches any unsynchronized access (AC1).
func TestConcurrentSetGetRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const goroutines = 16
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for g := 0; g < goroutines; g++ {
		chatID := int64(g)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := s.Set(chatID, fmt.Sprintf("sess-%d-%d", chatID, i)); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_, _ = s.Get(chatID)
			}
		}()
	}
	wg.Wait()

	// Each chat ends with some valid stored id; reopen confirms no corruption.
	if _, err := Open(path); err != nil {
		t.Fatalf("store corrupted under concurrency: %v", err)
	}
}
