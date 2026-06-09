//nolint:testpackage // intentionally whitebox to test unexported chat command-method internals
package chat

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// fakeDispatcher records Submit and Cancel calls without running anything, so a
// command test can assert that /new never submits a run and that /stop cancels
// the right chat. It satisfies the dispatcher interface.
type fakeDispatcher struct {
	mu        sync.Mutex
	submitted []ChatID // chatIDs passed to Submit, in order
	cancelled []ChatID // chatIDs passed to Cancel, in order
}

func (d *fakeDispatcher) Submit(chatID ChatID, _ func(ctx context.Context)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.submitted = append(d.submitted, chatID)
}

func (d *fakeDispatcher) Cancel(chatID ChatID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancelled = append(d.cancelled, chatID)
}

func (d *fakeDispatcher) submits() []ChatID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ChatID(nil), d.submitted...)
}

func (d *fakeDispatcher) cancels() []ChatID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ChatID(nil), d.cancelled...)
}

// newCommandService builds a Service over a fake dispatcher + the given session
// store so command tests can assert on Submit/Cancel/Delete without a live run.
func newCommandService(disp dispatcher, sess sessionStore) *Service {
	return New(Config{
		Runner:     &fakeRunner{},
		Transport:  newFakeChat(),
		Dispatcher: disp,
		Workspace:  &fakeWorkspace{},
		Sessions:   sess,
		Logger:     slog.New(slog.DiscardHandler),
	})
}

// TestNewSessionResets asserts /new deletes the chat's stored session (so the
// next message starts fresh) and that a reset of an absent chat is a no-op that
// still succeeds — AC2.
func TestNewSessionResets(t *testing.T) {
	disp := &fakeDispatcher{}
	sess := newFakeSessions()
	if err := sess.Set("100", "sess-seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := newCommandService(disp, sess)

	if err := svc.NewSession("100"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, ok := sess.get(); ok {
		t.Fatal("session 100 still present after NewSession")
	}
	if got := sess.deletes(); len(got) != 1 || got[0] != "100" {
		t.Fatalf("Delete log = %v, want [100]", got)
	}

	// Resetting an absent chat is a harmless no-op that still returns nil.
	if err := svc.NewSession("999"); err != nil {
		t.Fatalf("NewSession on absent chat: %v", err)
	}

	// /new must not submit a run (no Claude invocation).
	if got := disp.submits(); len(got) != 0 {
		t.Fatalf("NewSession submitted runs: %v", got)
	}
}

// TestNewSessionNilStore asserts /new is a safe no-op when continuity is disabled
// (nil store): no panic, no error — AC2.
func TestNewSessionNilStore(t *testing.T) {
	disp := &fakeDispatcher{}
	svc := newCommandService(disp, nil)
	if err := svc.NewSession("100"); err != nil {
		t.Fatalf("NewSession with nil store: %v", err)
	}
}

// TestStopChatCancels asserts /stop routes to dispatch.Cancel(chatID) and reports
// whether a run was active — AC3. With a registered in-flight run it returns true
// ("stopping" copy); with none it returns false ("nothing to stop") yet still
// issues the (no-op) Cancel via the SAME primitive as the inline Stop button.
func TestStopChatCancels(t *testing.T) {
	disp := &fakeDispatcher{}
	svc := newCommandService(disp, newFakeSessions())

	// No run in flight: StopChat reports false (nothing to stop) but still calls
	// Cancel(chatID) — a single, shared cancel path.
	if svc.StopChat("100") {
		t.Fatal("StopChat reported a run active when none was registered")
	}
	if got := disp.cancels(); len(got) != 1 || got[0] != "100" {
		t.Fatalf("Cancel log = %v, want [100]", got)
	}

	// Register an in-flight run for chat 100 (mimic run()'s bookkeeping).
	svc.mu.Lock()
	svc.runChat["7"] = "100"
	svc.mu.Unlock()

	if !svc.StopChat("100") {
		t.Fatal("StopChat reported no run active when one was registered")
	}
	if got := disp.cancels(); len(got) != 2 || got[1] != "100" {
		t.Fatalf("Cancel log = %v, want second cancel of 100", got)
	}
}
