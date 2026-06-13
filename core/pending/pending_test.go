package pending_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/duckbugio/flock/core/pending"
)

// TestEnqueueAssignsDistinctIDs asserts each Enqueue gets a fresh, distinct ID
// and the markers land in the chat's queue in submit order.
func TestEnqueueAssignsDistinctIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id1, err := s.Enqueue("100", pending.Marker{Prompt: "a"})
	if err != nil {
		t.Fatalf("Enqueue a: %v", err)
	}
	id2, err := s.Enqueue("100", pending.Marker{Prompt: "b"})
	if err != nil {
		t.Fatalf("Enqueue b: %v", err)
	}
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("expected distinct non-empty ids, got %q and %q", id1, id2)
	}
	q := s.All()["100"]
	if len(q) != 2 || q[0].Prompt != "a" || q[1].Prompt != "b" {
		t.Fatalf("queue = %+v, want ordered [a, b]", q)
	}
	if q[0].ID != id1 || q[1].ID != id2 {
		t.Fatalf("queue ids = [%q,%q], want [%q,%q]", q[0].ID, q[1].ID, id1, id2)
	}
}

// TestAllReturnsOrderedSlices asserts All preserves per-chat submit order and
// returns an independent copy (mutating it does not touch the store).
func TestAllReturnsOrderedSlices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, p := range []string{"a", "b", "c"} {
		if _, err := s.Enqueue("1", pending.Marker{Prompt: p}); err != nil {
			t.Fatalf("Enqueue %s: %v", p, err)
		}
	}
	all := s.All()
	q := all["1"]
	if len(q) != 3 || q[0].Prompt != "a" || q[1].Prompt != "b" || q[2].Prompt != "c" {
		t.Fatalf("queue = %+v, want ordered [a, b, c]", q)
	}
	// Mutating the returned copy must not affect the store.
	q[0].Prompt = "mutated"
	if again := s.All()["1"]; again[0].Prompt != "a" {
		t.Fatalf("All returned a shared slice; store mutated to %q", again[0].Prompt)
	}
}

// TestRemoveByID asserts Remove drops the exact entry, leaving its siblings, and
// is idempotent (a second Remove of the same id is a harmless no-op).
func TestRemoveByID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id1, _ := s.Enqueue("1", pending.Marker{Prompt: "a"})
	id2, _ := s.Enqueue("1", pending.Marker{Prompt: "b"})

	if err := s.Remove("1", id1); err != nil {
		t.Fatalf("Remove id1: %v", err)
	}
	q := s.All()["1"]
	if len(q) != 1 || q[0].ID != id2 || q[0].Prompt != "b" {
		t.Fatalf("after Remove id1, queue = %+v, want only b", q)
	}
	// Idempotent: removing the same id again is a no-op.
	if err := s.Remove("1", id1); err != nil {
		t.Fatalf("Remove id1 again: %v", err)
	}
	if len(s.All()["1"]) != 1 {
		t.Fatalf("idempotent Remove changed the queue: %+v", s.All()["1"])
	}
}

// TestRemoveDropsEmptyChatKey asserts removing the last marker in a chat's queue
// deletes the chat key entirely (so All() reports no entry for it).
func TestRemoveDropsEmptyChatKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := s.Enqueue("1", pending.Marker{Prompt: "only"})
	if err := s.Remove("1", id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.All()["1"]; ok {
		t.Fatalf("chat key not dropped after removing last marker: %+v", s.All())
	}
	// Removing from an absent chat is a no-op.
	if err := s.Remove("1", "nope"); err != nil {
		t.Fatalf("Remove absent chat: %v", err)
	}
}

// TestClearDropsWholeLane asserts Clear removes every marker for the chat at once
// and is a no-op for an absent chat.
func TestClearDropsWholeLane(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, _ = s.Enqueue("1", pending.Marker{Prompt: "a"})
	_, _ = s.Enqueue("1", pending.Marker{Prompt: "b"})
	_, _ = s.Enqueue("2", pending.Marker{Prompt: "c"})

	if err := s.Clear("1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := s.All()["1"]; ok {
		t.Fatalf("lane 1 not cleared: %+v", s.All())
	}
	if q := s.All()["2"]; len(q) != 1 || q[0].Prompt != "c" {
		t.Fatalf("Clear of lane 1 affected lane 2: %+v", q)
	}
	// Clearing an absent chat is a no-op.
	if err := s.Clear("999"); err != nil {
		t.Fatalf("Clear absent: %v", err)
	}
}

// TestSetAnchorUpdatesRightEntry asserts SetAnchor updates only the targeted
// marker and is a harmless no-op for an absent id.
func TestSetAnchorUpdatesRightEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id1, _ := s.Enqueue("1", pending.Marker{Prompt: "a"})
	id2, _ := s.Enqueue("1", pending.Marker{Prompt: "b"})

	if err := s.SetAnchor("1", id2, "anchor-b"); err != nil {
		t.Fatalf("SetAnchor: %v", err)
	}
	q := s.All()["1"]
	if q[0].ID != id1 || q[0].AnchorMsgID != "" {
		t.Fatalf("first marker anchor changed: %+v", q[0])
	}
	if q[1].ID != id2 || q[1].AnchorMsgID != "anchor-b" {
		t.Fatalf("second marker anchor not set: %+v", q[1])
	}
	// Absent id is a no-op.
	if err := s.SetAnchor("1", "nope", "x"); err != nil {
		t.Fatalf("SetAnchor absent id: %v", err)
	}
	if err := s.SetAnchor("absent", id1, "x"); err != nil {
		t.Fatalf("SetAnchor absent chat: %v", err)
	}
}

// TestReopenRoundTrip asserts an enqueued queue is reloaded by a fresh store
// opened on the same path (the restart path) and surfaced by All() in order.
func TestReopenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id1, _ := s.Enqueue("100", pending.Marker{Prompt: "first", StartedAt: 1})
	if err := s.SetAnchor("100", id1, "42"); err != nil {
		t.Fatalf("SetAnchor: %v", err)
	}
	id2, _ := s.Enqueue("100", pending.Marker{Prompt: "second", StartedAt: 2})

	s2, err := pending.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	q := s2.All()["100"]
	if len(q) != 2 {
		t.Fatalf("after reopen queue len = %d, want 2: %+v", len(q), q)
	}
	if q[0].ID != id1 || q[0].Prompt != "first" || q[0].AnchorMsgID != "42" || q[0].StartedAt != 1 {
		t.Fatalf("first marker mis-restored: %+v", q[0])
	}
	if q[1].ID != id2 || q[1].Prompt != "second" || q[1].StartedAt != 2 {
		t.Fatalf("second marker mis-restored: %+v", q[1])
	}
}

// TestCounterResumesAboveMaxPersistedID asserts that after a reopen, a new
// Enqueue yields an id strictly greater than every persisted id, so it can never
// collide with a persisted-but-not-yet-cleared marker the resume loop reuses.
func TestCounterResumesAboveMaxPersistedID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var maxID uint64
	for i := 0; i < 3; i++ {
		id, _ := s.Enqueue("1", pending.Marker{Prompt: strconv.Itoa(i)})
		n, _ := strconv.ParseUint(id, 10, 64)
		if n > maxID {
			maxID = n
		}
	}

	s2, err := pending.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	newID, err := s2.Enqueue("1", pending.Marker{Prompt: "after reopen"})
	if err != nil {
		t.Fatalf("Enqueue after reopen: %v", err)
	}
	n, err := strconv.ParseUint(newID, 10, 64)
	if err != nil {
		t.Fatalf("new id %q not numeric: %v", newID, err)
	}
	if n <= maxID {
		t.Fatalf("new id %d does not exceed max persisted id %d — collision risk", n, maxID)
	}
}

// TestEnqueueIsAtomicRewrite asserts Enqueue rewrites the whole file atomically
// (a second Enqueue for a different chat keeps the first) and leaves no temp
// files behind.
func TestEnqueueIsAtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json")
	s, err := pending.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Enqueue("1", pending.Marker{Prompt: "a"}); err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	if _, err := s.Enqueue("2", pending.Marker{Prompt: "b"}); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	all := s.All()
	if len(all) != 2 || all["1"][0].Prompt != "a" || all["2"][0].Prompt != "b" {
		t.Fatalf("after two Enqueues, all=%v want both lanes", all)
	}
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

// TestNilStoreSafe asserts a nil *FileStore behaves as a no-op store so callers
// can keep running when the store fails to open (best-effort startup wiring).
func TestNilStoreSafe(t *testing.T) {
	var s *pending.FileStore
	if id, err := s.Enqueue("1", pending.Marker{}); err != nil || id != "" {
		t.Fatalf("nil Enqueue = (%q, %v), want (\"\", nil)", id, err)
	}
	if err := s.SetAnchor("1", "1", "a"); err != nil {
		t.Fatalf("nil SetAnchor: %v", err)
	}
	if err := s.Remove("1", "1"); err != nil {
		t.Fatalf("nil Remove: %v", err)
	}
	if err := s.Clear("1"); err != nil {
		t.Fatalf("nil Clear: %v", err)
	}
	if got := s.All(); len(got) != 0 {
		t.Fatalf("nil All = %v, want empty", got)
	}
}

// TestOpenRejectsCorruptFile asserts a file that does not parse into the queue
// shape (e.g. a stale single-object format) surfaces as an Open error rather than
// being silently dual-decoded.
func TestOpenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	// The old single-object-per-chat format: a chat value that is an object, not
	// an array — it cannot decode into map[string][]Marker.
	if err := os.WriteFile(path, []byte(`{"100":{"prompt":"x"}}`), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := pending.Open(path); err == nil {
		t.Fatal("Open accepted a corrupt/old-format file; want a parse error")
	}
}
