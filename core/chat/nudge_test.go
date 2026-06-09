//nolint:testpackage // intentionally whitebox to test unexported telegram nudge internals
package chat

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/dispatch"
)

const (
	nudgeOwner = "duckbugio"
	nudgeRepo  = "flock"
)

// fakeStarrer is an in-memory starrer recording IsStarred/Star calls so a test
// can assert what the nudge did. starred is the value IsStarred reports; starErr
// (when set) makes Star fail.
type fakeStarrer struct {
	mu          sync.Mutex
	starred     bool
	isStarErr   error
	starErr     error
	isStarCalls int
	starCalls   int
}

func (f *fakeStarrer) IsStarred(_ context.Context, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isStarCalls++
	return f.starred, f.isStarErr
}

func (f *fakeStarrer) Star(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starCalls++
	if f.starErr != nil {
		return f.starErr
	}
	f.starred = true
	return nil
}

func (f *fakeStarrer) calls() (isStar, star int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isStarCalls, f.starCalls
}

// fakeNudgeStore is an in-memory nudgeStore.
type fakeNudgeStore struct {
	mu      sync.Mutex
	starred bool
}

func (s *fakeNudgeStore) Starred() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starred
}

func (s *fakeNudgeStore) MarkStarred() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starred = true
	return nil
}

// newNudgeService builds a Service wired to the star nudge over a real Dispatcher
// and fakes. cfg.Chat is the supplied fake chat.
func newNudgeService(
	t *testing.T, r claude.Runner, c Transport, cfg StarNudgeConfig,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		StarNudge:  cfg,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

func successRunner() *fakeRunner {
	return &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer, SessionID: "s1"}},
	}}
}

func enabledNudgeConfig(st *fakeStarrer, store nudgeStore) StarNudgeConfig {
	return StarNudgeConfig{
		Enabled: true,
		Owner:   nudgeOwner,
		Repo:    nudgeRepo,
		Client:  st,
		Store:   store,
	}
}

// TestNudgeFiresAfterSuccessWhenUnstarred asserts the nudge message is sent after
// a cleanly successful run when the repo is not starred.
func TestNudgeFiresAfterSuccessWhenUnstarred(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{starred: false}
	svc, d := newNudgeService(t, successRunner(), fc, enabledNudgeConfig(st, &fakeNudgeStore{}))
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	waitUntil(t, func() bool {
		return len(fc.sentNudges()) == 1
	})
}

// TestNudgeDoesNotFireOnError asserts a failed run (RunError) never nudges.
func TestNudgeDoesNotFireOnError(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{}
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.RunError, Err: errors.New("boom")},
	}}
	svc, d := newNudgeService(t, fr, fc, enabledNudgeConfig(st, &fakeNudgeStore{}))
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	// Wait for the error to be delivered, then assert no nudge / no API call.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return text != "" && text != anchorText
	})
	time.Sleep(20 * time.Millisecond)
	if got := fc.sentNudges(); len(got) != 0 {
		t.Fatalf("nudge fired on error run: %v", got)
	}
	if is, _ := st.calls(); is != 0 {
		t.Fatalf("IsStarred called on error run: %d", is)
	}
}

// TestNudgeMarksWhenAlreadyStarredAndSendsNothing asserts that when the API
// reports the repo already starred, the store is marked and no message is sent.
func TestNudgeMarksWhenAlreadyStarredAndSendsNothing(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{starred: true}
	store := &fakeNudgeStore{}
	svc, d := newNudgeService(t, successRunner(), fc, enabledNudgeConfig(st, store))
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	waitUntil(t, func() bool {
		return store.Starred()
	})
	if got := fc.sentNudges(); len(got) != 0 {
		t.Fatalf("nudge sent though already starred: %v", got)
	}
}

// TestNudgeSkippedWhenKnownStarred asserts a store already marked starred short-
// circuits the nudge entirely (no API call, no message).
func TestNudgeSkippedWhenKnownStarred(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{}
	store := &fakeNudgeStore{starred: true}
	svc, d := newNudgeService(t, successRunner(), fc, enabledNudgeConfig(st, store))
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return text == finalAnswer
	})
	time.Sleep(20 * time.Millisecond)
	if is, _ := st.calls(); is != 0 {
		t.Fatalf("IsStarred called though store already starred: %d", is)
	}
	if got := fc.sentNudges(); len(got) != 0 {
		t.Fatalf("nudge sent though store already starred: %v", got)
	}
}

// TestNudgeDisabledIsInert asserts a disabled nudge never touches the API or chat.
func TestNudgeDisabledIsInert(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{}
	svc, d := newNudgeService(t, successRunner(), fc, StarNudgeConfig{Enabled: false})
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return text == finalAnswer
	})
	time.Sleep(20 * time.Millisecond)
	if is, star := st.calls(); is != 0 || star != 0 {
		t.Fatalf("disabled nudge hit API: is=%d star=%d", is, star)
	}
	if got := fc.sentNudges(); len(got) != 0 {
		t.Fatalf("disabled nudge sent a message: %v", got)
	}
}

// TestStarPressStarsAndConfirms asserts a button press stars the repo, marks the
// store, and reports the confirmation toast.
func TestStarPressStarsAndConfirms(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{starred: false}
	store := &fakeNudgeStore{}
	svc, d := newNudgeService(t, successRunner(), fc, enabledNudgeConfig(st, store))
	defer d.Close()

	toast, ok := svc.StarPress()
	if !ok {
		t.Fatalf("StarPress ok = false, toast = %q", toast)
	}
	if toast != starDoneText {
		t.Errorf("toast = %q, want %q", toast, starDoneText)
	}
	if _, star := st.calls(); star != 1 {
		t.Errorf("Star calls = %d, want 1", star)
	}
	if !store.Starred() {
		t.Error("store not marked starred after successful press")
	}
}

// TestStarPressFailureIsGraceful asserts a failed Star reports the soft toast,
// does not mark the store, and never panics.
func TestStarPressFailureIsGraceful(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{starErr: errors.New("403 forbidden")}
	store := &fakeNudgeStore{}
	svc, d := newNudgeService(t, successRunner(), fc, enabledNudgeConfig(st, store))
	defer d.Close()

	toast, ok := svc.StarPress()
	if ok {
		t.Fatal("StarPress ok = true on Star failure, want false")
	}
	if toast != starFailToast {
		t.Errorf("toast = %q, want %q", toast, starFailToast)
	}
	if store.Starred() {
		t.Error("store marked starred despite Star failure")
	}
}

// TestStarPressDisabledConfirms asserts a press against a disabled nudge (a stale
// button on a redeployed/non-GitHub bot) confirms cleanly without an API call.
func TestStarPressDisabledConfirms(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{}
	svc, d := newNudgeService(t, successRunner(), fc, StarNudgeConfig{Enabled: false})
	defer d.Close()

	toast, ok := svc.StarPress()
	if !ok || toast != starDoneText {
		t.Fatalf("disabled StarPress = %q,%v; want %q,true", toast, ok, starDoneText)
	}
	if _, star := st.calls(); star != 0 {
		t.Errorf("disabled press hit Star API: %d", star)
	}
}

// TestNudgeRefiresEachUnstarredRun asserts the nudge fires again on a second
// successful run while the repo remains unstarred (re-nudge until resolved).
func TestNudgeRefiresEachUnstarredRun(t *testing.T) {
	fc := newFakeChat()
	st := &fakeStarrer{starred: false}
	svc, d := newNudgeService(t, successRunner(), fc, enabledNudgeConfig(st, &fakeNudgeStore{}))
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "first")
	waitUntil(t, func() bool { return len(fc.sentNudges()) == 1 })

	svc.Handle(context.Background(), testChatID, 100, "2", "second")
	waitUntil(t, func() bool { return len(fc.sentNudges()) == 2 })
}
