package nudge_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/duckbugio/flock/core/nudge"
)

func TestMissingFileIsUnstarred(t *testing.T) {
	t.Parallel()
	s, err := nudge.Open(filepath.Join(t.TempDir(), "nudge.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Starred() {
		t.Fatal("fresh store reports starred; want not starred")
	}
}

func TestMarkAndReloadAcrossRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nudge.json")

	s1, err := nudge.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s1.MarkStarred(); err != nil {
		t.Fatalf("MarkStarred: %v", err)
	}
	if !s1.Starred() {
		t.Fatal("after MarkStarred, Starred() = false")
	}

	// A fresh store opened from the SAME path must reflect the persisted flag,
	// proving the state survives a process restart.
	s2, err := nudge.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !s2.Starred() {
		t.Fatal("reopened store does not reflect MarkStarred")
	}
}

func TestMarkStarredIdempotent(t *testing.T) {
	t.Parallel()
	s, err := nudge.Open(filepath.Join(t.TempDir(), "nudge.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.MarkStarred(); err != nil {
		t.Fatalf("first MarkStarred: %v", err)
	}
	if err := s.MarkStarred(); err != nil {
		t.Fatalf("second MarkStarred: %v", err)
	}
	if !s.Starred() {
		t.Fatal("Starred() = false after marking")
	}
}

func TestNilStoreSafe(t *testing.T) {
	t.Parallel()
	var s *nudge.Store
	if s.Starred() {
		t.Fatal("nil store reports starred")
	}
	if err := s.MarkStarred(); err != nil {
		t.Fatalf("nil MarkStarred: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	s, err := nudge.Open(filepath.Join(t.TempDir(), "nudge.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = s.MarkStarred()
			_ = s.Starred()
		}()
	}
	wg.Wait()
	if !s.Starred() {
		t.Fatal("after concurrent marks, Starred() = false")
	}
}
