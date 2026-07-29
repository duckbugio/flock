//nolint:testpackage // whitebox: the gate is asserted through the Service's own run paths.
package chat

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/pending"
)

// countingRunner records how many runs actually reached the provider.
type countingRunner struct{ n atomic.Int64 }

func (r *countingRunner) Run(_ context.Context, _ string, _ agent.Options) (<-chan agent.Event, error) {
	r.n.Add(1)
	out := make(chan agent.Event)
	go func() {
		defer close(out)
		out <- agent.Event{Type: agent.Result, Result: &agent.RunResult{Text: "done", Subtype: "success"}}
	}()
	return out, nil
}

func (r *countingRunner) calls() int64 { return r.n.Load() }

// stubGate is a RunGate whose verdict a test flips at will, standing in for
// codexauth.Manager. It counts consultations, which is the precise signal a run
// reached the gate — letting a test assert "nothing ran" without racing.
type stubGate struct {
	blocked atomic.Bool
	n       atomic.Int64
	told    stubTold
}

// stubTold mirrors the real gate's "told once per destination" registry.
type stubTold struct {
	mu   sync.Mutex
	told map[string]bool
}

func (s *stubTold) Should(dest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.told == nil {
		s.told = map[string]bool{}
	}
	if s.told[dest] {
		return false
	}
	s.told[dest] = true
	return true
}

func (s *stubTold) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.told)
}

func (g *stubGate) Blocked() bool {
	g.n.Add(1)
	return g.blocked.Load()
}

func (g *stubGate) NoticeFor(dest string) string {
	if !g.blocked.Load() {
		g.told.Clear()
		return ""
	}
	if !g.told.Should(dest) {
		return ""
	}
	return gateNotice
}

func (g *stubGate) calls() int64 { return g.n.Load() }

// gateNotice is the text stubGate blocks with.
const gateNotice = "Codex is not authorized yet. Send /login."

// notices counts how many times the gate's notice was delivered, ignoring the
// ordinary run output an authorized run also sends.
func notices(c *fakeChat) int {
	n := 0
	for _, text := range allSent(c) {
		if text == gateNotice {
			n++
		}
	}
	return n
}

// blockingGate returns a gate that starts out blocking.
func blockingGate() *stubGate {
	g := &stubGate{}
	g.blocked.Store(true)
	return g
}

// gatedService builds a Service whose runs pass through gate.
func gatedService(t *testing.T, r agent.Runner, c Transport, gate RunGate, p pendingStore) *Service {
	t.Helper()
	d := dispatch.New(4)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = d.Shutdown(ctx)
	})
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Pending:    p,
		Auth:       gate,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s
}

// TestGateBlocksEveryRunSource is the point of gating in the Service rather than
// only in the adapters: a user message is just one of four ways a run starts
// here. A cron fire, a poller relay and the restart replay never touch an
// adapter, so an adapter-only gate would march all of them into a CLI that
// cannot authenticate — once per schedule tick, with nothing user-visible.
func TestGateBlocksEveryRunSource(t *testing.T) {
	tests := []struct {
		name  string
		start func(s *Service)
	}{
		{"user message", func(s *Service) { s.Handle(context.Background(), testChatID, 7, "m1", "build it") }},
		{"poller relay", func(s *Service) { s.Inject(testChatID, "a reviewer commented") }},
		{"cron fire", func(s *Service) { s.InjectScheduled(testChatID, "nightly", 7) }},
		{"restart replay", func(s *Service) { s.ResumePending(testChatID, pending.Marker{ID: "p1", Prompt: "resume me"}) }},
		{"autonomy follow-up", func(s *Service) { s.InjectAuto(context.Background(), testChatID, "ci went red") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, r, gate := newFakeChat(), &countingRunner{}, blockingGate()
			s := gatedService(t, r, c, gate, nil)

			tt.start(s)
			waitUntil(t, func() bool { return gate.calls() >= 1 })

			if got := r.calls(); got != 0 {
				t.Errorf("the provider ran %d times while unauthorized, want 0", got)
			}
			waitUntil(t, func() bool { return notices(c) == 1 })
		})
	}
}

// TestGateNotifiesOnceUntilAuthorized: background sources recur (a five-minute
// cron would otherwise post a notice every five minutes), so the chat is told
// once — and told again after a later lapse, or a returning outage would go
// unexplained.
func TestGateNotifiesOnceUntilAuthorized(t *testing.T) {
	c, gate := newFakeChat(), blockingGate()
	s := gatedService(t, &countingRunner{}, c, gate, nil)

	s.Inject(testChatID, "first")
	s.Inject(testChatID, "second")
	s.Inject(testChatID, "third")
	waitUntil(t, func() bool { return gate.calls() >= 3 })

	if got := notices(c); got != 1 {
		t.Fatalf("sent %d notices for three blocked runs, want 1: %v", got, allSent(c))
	}

	// Authorized again — the "already told them" memory must reset.
	gate.blocked.Store(false)
	s.Inject(testChatID, "now allowed")
	waitUntil(t, func() bool { return gate.calls() >= 4 })

	// ...and a fresh lapse is announced again.
	gate.blocked.Store(true)
	s.Inject(testChatID, "blocked again")
	waitUntil(t, func() bool { return notices(c) == 2 })
}

// TestGateKeepsThePendingMarker: a blocked replay must NOT clear the marker of
// the run it refused, or an interrupted run would be lost to the very outage
// that stopped it. The marker survives to be replayed after the sign-in.
func TestGateKeepsThePendingMarker(t *testing.T) {
	c, p, gate := newFakeChat(), newFakePending(), blockingGate()
	s := gatedService(t, &countingRunner{}, c, gate, p)

	if _, err := p.Enqueue(testChatID, pending.Marker{ID: "p1", Prompt: "resume me"}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	s.ResumePending(testChatID, pending.Marker{ID: "p1", Prompt: "resume me"})
	waitUntil(t, func() bool { return gate.calls() >= 1 })

	if got := p.removes(); len(got) != 0 {
		t.Errorf("a blocked run cleared the pending marker (%v); the interrupted work is lost", got)
	}
	if p.count() != 1 {
		t.Errorf("pending markers = %d, want the interrupted run still queued", p.count())
	}
}

// TestGateAllowsRunsWhenAuthorized keeps the gate honest: an authorized gate —
// or none at all, which is every non-Codex deployment — must not change behavior.
func TestGateAllowsRunsWhenAuthorized(t *testing.T) {
	for _, tt := range []struct {
		name string
		gate RunGate
	}{
		{"authorized gate", &stubGate{}},
		{"no gate wired", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, r := newFakeChat(), &countingRunner{}
			s := gatedService(t, r, c, tt.gate, nil)

			s.Handle(context.Background(), testChatID, 7, "m1", "build it")
			waitUntil(t, func() bool { return r.calls() == 1 })
		})
	}
}

// TestBlockedSubmitLeavesNoPendingMarker is the marker-leak regression. The
// submit paths enqueue a marker BEFORE handing the job to the dispatcher, and a
// blocked run can never reach the clean terminal that clears it — so blocking
// inside the run would strand one marker per refused message. The pending store
// is append-only, and a poller or cron firing into an unauthorized deployment
// would grow it without bound and replay every marker on the next restart.
func TestBlockedSubmitLeavesNoPendingMarker(t *testing.T) {
	tests := []struct {
		name  string
		start func(s *Service)
	}{
		{"user message", func(s *Service) { s.Handle(context.Background(), testChatID, 7, "m1", "build it") }},
		{"edited message", func(s *Service) { s.HandleEdit(context.Background(), testChatID, 7, "m1", "edited") }},
		{"poller relay", func(s *Service) { s.Inject(testChatID, "a reviewer commented") }},
		{"autonomy follow-up", func(s *Service) { s.InjectAuto(context.Background(), testChatID, "ci went red") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, p, gate := newFakeChat(), newFakePending(), blockingGate()
			s := gatedService(t, &countingRunner{}, c, gate, p)

			for range 5 {
				tt.start(s)
			}
			waitUntil(t, func() bool { return gate.calls() >= 5 })

			if got := p.count(); got != 0 {
				t.Errorf("five blocked submits left %d pending markers, want 0", got)
			}
		})
	}
}

// TestBlockedScheduledFireIsReportedDropped: the scheduler reads the bool to know
// the fire did not take, exactly as it does for the cost cap.
func TestBlockedScheduledFireIsReportedDropped(t *testing.T) {
	c, gate := newFakeChat(), blockingGate()
	s := gatedService(t, &countingRunner{}, c, gate, newFakePending())

	if s.InjectScheduled(testChatID, "nightly", 7) {
		t.Error("InjectScheduled reported the fire enqueued while unauthorized")
	}
}

// TestNotifyResetIsProcessWide: authorization is process-wide, so one chat
// submitting after recovery proves the outage is over for all of them. Clearing
// only the submitting chat would leave a chat that stayed quiet through the
// recovery window silently un-notified when the next lapse hit it.
func TestNotifyResetIsProcessWide(t *testing.T) {
	c, gate := newFakeChat(), blockingGate()
	s := gatedService(t, &countingRunner{}, c, gate, newFakePending())

	// The quiet chat is told once, then says nothing more.
	s.Inject(testChatID, "first")
	waitUntil(t, func() bool { return notices(c) == 1 })

	// Recovery happens while ANOTHER chat submits; the quiet chat stays silent.
	gate.blocked.Store(false)
	s.Inject("other-chat", "now allowed")
	waitUntil(t, func() bool { return gate.calls() >= 2 })

	// The next lapse must be announced to the quiet chat again.
	gate.blocked.Store(true)
	s.Inject(testChatID, "blocked again")
	waitUntil(t, func() bool { return notices(c) == 2 })
}

// TestOneNoticeAcrossLayers is the composition regression the per-layer tests
// could not see. The refusal reaches a chat from TWO places — core/chat refusing
// a background submission, and an adapter answering a user's message — and each
// layer used to keep its own "already told them" registry. Each was correct
// alone; together they told one chat the same sentence twice, once for the
// poller's refusal and once for the user's next message.
func TestOneNoticeAcrossLayers(t *testing.T) {
	c, gate := newFakeChat(), blockingGate()
	s := gatedService(t, &countingRunner{}, c, gate, newFakePending())

	// The background path refuses first and explains.
	s.Inject(testChatID, "a reviewer commented")
	waitUntil(t, func() bool { return notices(c) == 1 })

	// The adapter path asks the SAME gate about the same chat, and must get nothing
	// to send — this chat has already been told.
	if notice := gate.NoticeFor(testChatID); notice != "" {
		t.Errorf("the adapter would have repeated the notice: %q", notice)
	}
}
