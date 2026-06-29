//nolint:testpackage // intentionally whitebox to test unexported telegram cost recording internals
package chat

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
)

// newTestServiceWithCosts builds a Service wired to a real cost store so the
// post-run cost-record hook can be exercised end to end.
func newTestServiceWithCosts(t *testing.T, r agent.Runner, c Transport, costs *cost.Store) (*Service, *dispatch.Dispatcher) {
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
	fr := &fakeRunner{events: []agent.Event{
		{Type: agent.Result, Result: &agent.RunResult{Text: "done", CostUSD: 0.5}},
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
	fr := &fakeRunner{events: []agent.Event{
		{Type: agent.RunError, Err: errors.New("boom")},
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

// newScheduledTestService builds a Service wired to a cost store (with the given
// per-user cap), a fake pending store, and a real Dispatcher — the harness for the
// InjectScheduled tests, which assert the cost gate and the ephemeral (no-marker)
// behavior together.
func newScheduledTestService(
	t *testing.T, r agent.Runner, c Transport, costs *cost.Store, capUSD float64, p pendingStore,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Costs:      costs,
		CostCapUSD: capUSD,
		Pending:    p,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// TestInjectScheduledUnderCapRunsEphemerally asserts a scheduled fire under the
// cost cap is accepted (returns true), actually runs, and — unlike Inject — writes
// NO pending marker (it must never auto-resume on restart).
func TestInjectScheduledUnderCapRunsEphemerally(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []agent.Event{
		{Type: agent.Result, Result: &agent.RunResult{Text: "scheduled done", CostUSD: 0.1}},
	}}
	costs, err := cost.Open(t.TempDir() + "/costs.json")
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	pend := newFakePending()
	svc, d := newScheduledTestService(t, fr, fc, costs, 5.0, pend)
	defer d.Close()

	const creator int64 = 42
	if !svc.InjectScheduled("100", "go", creator) {
		t.Fatal("InjectScheduled returned false under the cap, want true")
	}
	// The run reaches its terminal and delivers its answer.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return strings.Contains(text, "scheduled done")
	})
	// EPHEMERAL: no pending marker may ever exist for this chat (Inject would have
	// enqueued one). Poll briefly so a late stray enqueue would still be caught.
	for i := 0; i < 10; i++ {
		if pend.count() != 0 {
			t.Fatalf("a scheduled fire wrote %d pending markers, want 0 (ephemeral)", pend.count())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestInjectScheduledOverCapDropsRun asserts a scheduled fire for a creator already
// over the cost cap is denied (returns false), runs nothing, and writes no marker.
func TestInjectScheduledOverCapDropsRun(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []agent.Event{
		{Type: agent.Result, Result: &agent.RunResult{Text: "must not run", CostUSD: 0.1}},
	}}
	costs, err := cost.Open(t.TempDir() + "/costs.json")
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	const creator int64 = 42
	// Push the creator over a tiny cap before the fire.
	if err := costs.Add(creator, 1.0); err != nil {
		t.Fatalf("seed cost: %v", err)
	}
	pend := newFakePending()
	svc, d := newScheduledTestService(t, fr, fc, costs, 0.5, pend)
	defer d.Close()

	if svc.InjectScheduled("100", "go", creator) {
		t.Fatal("InjectScheduled returned true for a creator over the cap, want false")
	}
	// Nothing was submitted: no run delivers, and no marker is written. Give a (would-
	// be) run ample time to wrongly start.
	time.Sleep(50 * time.Millisecond)
	if text, _ := fc.snapshot(); strings.Contains(text, "must not run") {
		t.Fatal("an over-cap scheduled fire ran a job")
	}
	if pend.count() != 0 {
		t.Fatalf("an over-cap scheduled fire wrote %d markers, want 0", pend.count())
	}
}
