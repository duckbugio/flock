//nolint:testpackage // intentionally whitebox to test unexported telegram cost recording internals
package chat

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
)

// newTestServiceWithCosts builds a Service wired to a real cost store so the
// post-run cost-record hook can be exercised end to end.
func newTestServiceWithCosts(t *testing.T, r claude.Runner, c Transport, costs *cost.Store) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Costs:      costs,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// TestRunRecordsCostOnResult asserts a completed run with a Result accumulates
// its CostUSD against the sending user (the per-user cumulative cap input).
func TestRunRecordsCostOnResult(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.Result, Result: &claude.RunResult{Text: "done", CostUSD: 0.5}},
	}}
	costs, err := cost.Open(t.TempDir() + "/costs.json")
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	svc, d := newTestServiceWithCosts(t, fr, fc, costs)
	defer d.Close()

	const user int64 = 42
	svc.Handle(context.Background(), "100", user, "1", "go")

	// Once the run's cost is recorded, a 0.5 cap is reached (>=) and denies; a 1.0
	// cap still allows — proving exactly 0.5 was added for THIS user.
	waitUntil(t, func() bool { return !costs.Allowed(user, 0.5) })
	if !costs.Allowed(user, 1.0) {
		t.Fatal("recorded cost should be exactly 0.5 (under a 1.0 cap)")
	}
	// A different user accrued nothing.
	if !costs.Allowed(99, 0.0001) {
		t.Fatal("cost must be attributed to the sending user only")
	}
}

// TestRunRecordsZeroCostOnError asserts an error/no-Result run records ZERO cost
// (finalResult is nil), so a failed run never charges the user.
func TestRunRecordsZeroCostOnError(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.RunError, Err: errors.New("boom")},
	}}
	costs, err := cost.Open(t.TempDir() + "/costs.json")
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	svc, d := newTestServiceWithCosts(t, fr, fc, costs)
	defer d.Close()

	const user int64 = 7
	svc.Handle(context.Background(), "100", user, "1", "go")

	// Wait for the terminal error to surface, then assert nothing was charged: even
	// a tiny positive cap still allows.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return strings.Contains(text, "boom")
	})
	if !costs.Allowed(user, 0.0001) {
		t.Fatal("an error run must record zero cost (no Result)")
	}
}
