package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/session"
	"github.com/duckbugio/flock/internal/tgui"
)

// TestRetryAfter checks the Telegram-429 detector that drives the rate-limit
// back-off: a TooManyRequestsError yields its RetryAfter (zero floored to 1s);
// any other error yields no back-off.
func TestRetryAfter(t *testing.T) {
	if d, ok := retryAfter(&bot.TooManyRequestsError{RetryAfter: 19}); !ok || d != 19*time.Second {
		t.Fatalf("retryAfter(429, 19) = %v, %v; want 19s, true", d, ok)
	}
	if d, ok := retryAfter(&bot.TooManyRequestsError{RetryAfter: 0}); !ok || d != time.Second {
		t.Fatalf("retryAfter(429, 0) = %v, %v; want 1s, true", d, ok)
	}
	if _, ok := retryAfter(errors.New("boom")); ok {
		t.Fatal("retryAfter(non-429) reported a back-off")
	}
}

// openRealStore opens the real file-backed session store; it is a thin alias so
// the test body reads clearly.
func openRealStore(path string) (*session.FileStore, error) {
	return session.Open(path)
}

// fakeChat records the sequence of Telegram operations and lets a test inspect
// the final state. It is safe for concurrent use because edits arrive from the
// ticker goroutine and the run goroutine.
type fakeChat struct {
	mu       sync.Mutex
	nextID   int
	texts    map[int]string // current text per message id
	hasStop  map[int]bool   // whether the message currently shows a Stop button
	deleted  map[int]bool
	sent     []string // every Send'd text, in order
	docs     []string // every SendDocument'd filename, in order
	drafts   []string // every successful StreamDraft'd text, in order (live preview)
	draftErr error    // when set, StreamDraft returns it (simulates no draft support)
}

func newFakeChat() *fakeChat {
	return &fakeChat{
		texts:   map[int]string{},
		hasStop: map[int]bool{},
		deleted: map[int]bool{},
	}
}

func (f *fakeChat) Send(_ context.Context, _ int64, text, stopRunID string, _ bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	f.texts[id] = text
	f.hasStop[id] = stopRunID != ""
	f.sent = append(f.sent, text)
	return id, nil
}

func (f *fakeChat) Edit(_ context.Context, _ int64, messageID int, text, stopRunID string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts[messageID] = text
	f.hasStop[messageID] = stopRunID != ""
	return nil
}

func (f *fakeChat) StreamDraft(_ context.Context, _ int64, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.draftErr != nil {
		return f.draftErr
	}
	f.drafts = append(f.drafts, text)
	return nil
}

func (f *fakeChat) Delete(_ context.Context, _ int64, messageID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[messageID] = true
	return nil
}

func (f *fakeChat) SendDocument(_ context.Context, _ int64, name string, data io.Reader) error {
	// Drain the reader so the fake behaves like a real upload (and so the source
	// file is read to completion before the sweeper closes it).
	_, _ = io.Copy(io.Discard, data)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, name)
	return nil
}

func (f *fakeChat) sentDocs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.docs...)
}

func (f *fakeChat) snapshot(id int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.texts[id], f.hasStop[id]
}

// fakeWorkspace returns a fixed path and records the chat ids it was asked for.
type fakeWorkspace struct {
	mu    sync.Mutex
	calls []int64
	err   error
}

func (w *fakeWorkspace) Ensure(chatID int64) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, chatID)
	if w.err != nil {
		return "", w.err
	}
	return "/tmp/chat_" + strconv.FormatInt(chatID, 10), nil
}

// fakeRunner emits a scripted slice of events on a channel the test controls.
// When gate is non-nil it waits on it (or ctx.Done) before sending the terminal
// event, letting a test cancel the context mid-run.
type fakeRunner struct {
	events []claude.Event
	gate   chan struct{} // if non-nil, wait for close before the terminal event
}

func (r *fakeRunner) Run(ctx context.Context, _ string, _ claude.Options) (<-chan claude.Event, error) {
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		for _, e := range r.events {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
		if r.gate != nil {
			select {
			case <-r.gate:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// fakeSessions is an in-memory sessionStore for adapter tests. It records both
// the stored ids and the order of Set calls so a test can assert what was
// persisted (and when).
type fakeSessions struct {
	mu     sync.Mutex
	store  map[int64]string
	setLog []string // sessionIDs passed to Set, in order
	delLog []int64  // chatIDs passed to Delete, in order
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{store: map[int64]string{}}
}

func (f *fakeSessions) Get(chatID int64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.store[chatID]
	return id, ok
}

func (f *fakeSessions) Set(chatID int64, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID == "" {
		return nil
	}
	f.store[chatID] = sessionID
	f.setLog = append(f.setLog, sessionID)
	return nil
}

func (f *fakeSessions) Delete(chatID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, chatID)
	f.delLog = append(f.delLog, chatID)
	return nil
}

func (f *fakeSessions) get(chatID int64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.store[chatID]
	return id, ok
}

func (f *fakeSessions) deletes() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.delLog...)
}

func (f *fakeSessions) sets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setLog...)
}

// newTestService builds a Service over a real Dispatcher and a fake workspace.
func newTestService(t *testing.T, r claude.Runner, c chat) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Chat:       c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	s.tick = 5 * time.Millisecond // speed up the wall-clock ticker for tests
	return s, d
}

// newTestServiceWithSessions builds a Service wired to a session store (and an
// optional per-run timeout) for the Stage 5 continuity/timeout tests.
func newTestServiceWithSessions(t *testing.T, r claude.Runner, c chat, sess sessionStore, timeout time.Duration) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Chat:       c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Sessions:   sess,
		Timeout:    timeout,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// sessionRunner is a fake Runner that records the SessionID it was invoked with
// (to assert --resume) and emits a scripted SystemInit + Result. When gate is
// non-nil it blocks AFTER SystemInit until released or the ctx is cancelled,
// letting a test time out / stop the run between init and result.
type sessionRunner struct {
	mu        sync.Mutex
	gotIDs    []string      // Options.SessionID per Run call, in order
	emitInit  string        // session_id to emit on SystemInit
	emitFinal string        // session_id on the terminal Result (may differ)
	gate      chan struct{} // if non-nil, block after SystemInit until closed/cancelled
}

func (r *sessionRunner) Run(ctx context.Context, _ string, o claude.Options) (<-chan claude.Event, error) {
	r.mu.Lock()
	r.gotIDs = append(r.gotIDs, o.SessionID)
	r.mu.Unlock()

	out := make(chan claude.Event)
	go func() {
		defer close(out)
		if !emit(ctx, out, claude.Event{Type: claude.SystemInit, SessionID: r.emitInit}) {
			return
		}
		if r.gate != nil {
			select {
			case <-r.gate:
			case <-ctx.Done():
				return // cancelled/timed out after SystemInit, before Result
			}
		}
		emit(ctx, out, claude.Event{Type: claude.Result, Result: &claude.RunResult{Text: "done", SessionID: r.emitFinal}})
	}()
	return out, nil
}

func (r *sessionRunner) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.gotIDs...)
}

// emit mirrors the run loop's blocking-send-or-cancel for the fake runner.
func emit(ctx context.Context, out chan<- claude.Event, e claude.Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

func TestRunRendersProgressThenFinal(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.ToolUse, Tool: "Bash"},
		{Type: claude.Text, Text: "all done"},
		{Type: claude.Result, Result: &claude.RunResult{Text: "final answer", SessionID: "s1"}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "hello")

	// The progress message (id 1) should eventually hold the final answer with no
	// Stop button.
	waitUntil(t, func() bool {
		text, stop := fc.snapshot(1)
		return text == "final answer" && !stop
	})
}

// TestRunStreamsProgressAsDraftThenClears asserts live progress is pushed via the
// rate-limit-free draft preview (not by editing the anchor), and that the draft is
// cleared when the answer is finalized into the anchor.
func TestRunStreamsProgressAsDraftThenClears(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.ToolUse, Tool: "Bash"},
		{Type: claude.Result, Result: &claude.RunResult{Text: "final answer", SessionID: "s1"}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "hello")

	waitUntil(t, func() bool {
		text, stop := fc.snapshot(1)
		return text == "final answer" && !stop
	})

	fc.mu.Lock()
	defer fc.mu.Unlock()
	// The anchor was sent as the fixed Stop-bearing anchor text — progress did NOT
	// ride it (it streams as drafts).
	if len(fc.sent) == 0 || !strings.Contains(fc.sent[0], "Working") {
		t.Fatalf("anchor not sent as the Working anchor; sent=%v", fc.sent)
	}
	// Live progress streamed via at least one draft preview...
	if len(fc.drafts) == 0 || !strings.Contains(fc.drafts[0], "Working") {
		t.Fatalf("progress not streamed as a draft; drafts=%v", fc.drafts)
	}
	// ...and the draft is cleared (empty) once the answer is finalized.
	if last := fc.drafts[len(fc.drafts)-1]; last != "" {
		t.Fatalf("draft not cleared on finish; last draft = %q", last)
	}
}

// TestRunFallsBackToEditsWhenDraftFails asserts that if draft streaming is
// unsupported (StreamDraft errors), the run still delivers the answer by editing
// the anchor — nothing is lost and no draft is recorded.
func TestRunFallsBackToEditsWhenDraftFails(t *testing.T) {
	fc := newFakeChat()
	fc.draftErr = errors.New("drafts unsupported")
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.ToolUse, Tool: "Bash"},
		{Type: claude.Result, Result: &claude.RunResult{Text: "final answer"}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "hello")

	// Despite drafts failing, the answer still lands — delivered by editing the anchor.
	waitUntil(t, func() bool {
		text, stop := fc.snapshot(1)
		return text == "final answer" && !stop
	})

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.drafts) != 0 {
		t.Fatalf("no draft should succeed in fallback mode; got %v", fc.drafts)
	}
}

func TestRunChunksLongFinal(t *testing.T) {
	long := strings.Repeat("x", tgui.TelegramMaxMessage+500)
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.Result, Result: &claude.RunResult{Text: long}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()
	svc.Handle(context.Background(), 100, 100, 1, "go")

	// Wait until the tail chunk has been sent (2 sends: progress + tail).
	waitUntil(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.sent) == 2
	})

	// First chunk replaces the progress message; the remainder is a new Send.
	first, _ := fc.snapshot(1)
	if utf8.RuneCountInString(first) > tgui.TelegramMaxMessage {
		t.Fatalf("first chunk too big: %d", utf8.RuneCountInString(first))
	}
}

func TestRunErrorEvent(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.RunError, Err: errors.New("boom")},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()
	svc.Handle(context.Background(), 100, 100, 1, "go")

	waitUntil(t, func() bool {
		text, _ := fc.snapshot(1)
		return strings.Contains(text, "boom")
	})
}

func TestStopCancelsRun(t *testing.T) {
	fc := newFakeChat()
	gate := make(chan struct{})
	fr := &fakeRunner{
		events: []claude.Event{{Type: claude.ToolUse, Tool: "Bash"}},
		gate:   gate, // run hangs after the tool_use until ctx is cancelled
	}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "go")

	// Wait until the run is registered, then Stop it. runID for the first run is "1".
	waitUntil(t, func() bool { return svc.Stop("1") })

	waitUntil(t, func() bool {
		text, stop := fc.snapshot(1)
		return strings.Contains(text, "Stopped") && !stop
	})
	close(gate) // unblock any lingering goroutine
}

func TestWorkspaceErrorSurfaced(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{}
	d := dispatch.New(4)
	defer d.Close()
	svc := New(Config{
		Runner:     fr,
		Chat:       fc,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{err: errors.New("no workspace")},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	svc.tick = 5 * time.Millisecond

	svc.Handle(context.Background(), 100, 100, 1, "go")
	waitUntil(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		for _, s := range fc.sent {
			if strings.Contains(s, "no workspace") {
				return true
			}
		}
		return false
	})
}

func TestSerialWithinChat(t *testing.T) {
	fc := newFakeChat()
	gate := make(chan struct{})
	fr := &fakeRunner{gate: gate}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	// First run blocks on its gate; a second message for the same chat must queue
	// behind it (serial), so only one progress message exists while the first runs.
	svc.Handle(context.Background(), 100, 100, 1, "first")
	waitUntil(t, func() bool {
		_, stop := fc.snapshot(1)
		return stop // first run posted its progress message
	})

	svc.Handle(context.Background(), 100, 100, 2, "second")
	// The second run must not have started while the first is gated: no second
	// progress message (message id 2) should exist yet.
	time.Sleep(30 * time.Millisecond)
	fc.mu.Lock()
	_, secondStarted := fc.texts[2]
	fc.mu.Unlock()
	if secondStarted {
		t.Fatal("second run started before the first finished — not serial")
	}

	close(gate) // let both runs complete
}

// recordingRunner is a Runner that records the prompts it was asked to run and
// blocks each run on a shared gate until the test releases it, so a test can
// observe a run as "in-flight" and then supersede it.
type recordingRunner struct {
	mu      sync.Mutex
	prompts []string
	gate    chan struct{} // closed by the test to let runs finish
}

func (r *recordingRunner) Run(ctx context.Context, prompt string, _ claude.Options) (<-chan claude.Event, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		// Hang until released or cancelled (an edit supersede cancels the ctx).
		select {
		case <-r.gate:
		case <-ctx.Done():
			return
		}
		out <- claude.Event{Type: claude.Result, Result: &claude.RunResult{Text: "done: " + prompt}}
	}()
	return out, nil
}

func (r *recordingRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

// TestHandleEditSupersedesInFlightRun asserts that editing a message whose run
// is in-flight cancels that run and starts a fresh one with the new text (AC2).
func TestHandleEditSupersedesInFlightRun(t *testing.T) {
	fc := newFakeChat()
	rr := &recordingRunner{gate: make(chan struct{})}
	svc, d := newTestService(t, rr, fc)
	defer d.Close()

	// Original message M (id 7) starts a run that hangs.
	svc.Handle(context.Background(), 100, 100, 7, "first version")
	waitUntil(t, func() bool {
		return len(rr.seen()) == 1 && rr.seen()[0] == "first version"
	})

	// An edit of the SAME message id supersedes: cancel the in-flight run and
	// resubmit with the new text.
	svc.HandleEdit(context.Background(), 100, 100, 7, "edited version")

	// The edited run eventually starts (serial: it runs after the cancelled one
	// unwinds).
	waitUntil(t, func() bool {
		seen := rr.seen()
		return len(seen) == 2 && seen[1] == "edited version"
	})

	close(rr.gate) // let the edited run finish

	// The final delivered text comes from the edited prompt, not the original.
	// The result replaces a progress message via Edit, so scan the current text
	// of every message.
	waitUntil(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		for _, s := range fc.texts {
			if strings.Contains(s, "edited version") {
				return true
			}
		}
		return false
	})
}

// TestHandleEditOlderMessageDoesNotRewindLatest asserts that, with a newer
// message's run in flight, editing an OLDER message does NOT rewind the chat's
// recorded "latest" message id — so a subsequent edit of the newer message still
// supersedes its in-flight run instead of queuing a duplicate.
func TestHandleEditOlderMessageDoesNotRewindLatest(t *testing.T) {
	fc := newFakeChat()
	rr := &recordingRunner{gate: make(chan struct{})}
	svc, d := newTestService(t, rr, fc)
	defer d.Close()

	// Newer message N (id 20) starts a run that hangs in-flight.
	svc.Handle(context.Background(), 100, 100, 20, "newer version")
	waitUntil(t, func() bool {
		return len(rr.seen()) == 1 && rr.seen()[0] == "newer version"
	})

	// An edit of an OLDER message (id 5) arrives. It is treated as a fresh run
	// (not a supersede) and must NOT rewind lastMsg back to 5. The fresh run is
	// queued behind the still-hanging newer run (serial per chat), so we assert on
	// lastMsg directly rather than waiting for the queued run to start.
	svc.HandleEdit(context.Background(), 100, 100, 5, "older edit")

	svc.mu.Lock()
	last := svc.lastMsg[100]
	svc.mu.Unlock()
	if last != 20 {
		t.Fatalf("lastMsg rewound to %d, want 20 (newer message must stay latest)", last)
	}

	// Now editing the newer message (id 20) must still supersede its in-flight
	// run: because lastMsg was not rewound, this matches the latest and cancels
	// the in-flight run. With lastMsg left at 5 (the bug) this edit would instead
	// queue a duplicate run.
	svc.HandleEdit(context.Background(), 100, 100, 20, "newer edited")

	close(rr.gate) // let queued runs drain
	waitUntil(t, func() bool {
		for _, p := range rr.seen() {
			if p == "newer edited" {
				return true
			}
		}
		return false
	})
}

// TestHandleEditUnknownMessageStartsFreshRun asserts that an edit for a message
// that was never submitted (unknown to the Service) is handled gracefully by
// starting a fresh run, so the user still gets an answer (AC2).
func TestHandleEditUnknownMessageStartsFreshRun(t *testing.T) {
	fc := newFakeChat()
	rr := &recordingRunner{gate: make(chan struct{})}
	close(rr.gate) // runs finish immediately
	svc, d := newTestService(t, rr, fc)
	defer d.Close()

	// No prior Handle for this chat/message: editing message 99 still runs.
	svc.HandleEdit(context.Background(), 200, 200, 99, "out of nowhere")

	waitUntil(t, func() bool {
		seen := rr.seen()
		return len(seen) == 1 && seen[0] == "out of nowhere"
	})
}

// TestSessionPersistedAndResumed asserts a run's session_id is persisted to the
// store, and that a SUBSEQUENT run for the same chat passes the stored id as
// Options.SessionID (resume) — AC2.
func TestSessionPersistedAndResumed(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	fr := &sessionRunner{emitInit: "sess-xyz", emitFinal: "sess-xyz"}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)
	defer d.Close()

	// First run for chat 100: no stored id yet, so it starts fresh and the
	// captured session_id is persisted.
	svc.Handle(context.Background(), 100, 100, 1, "hello")
	waitUntil(t, func() bool {
		id, ok := sess.get(100)
		return ok && id == "sess-xyz"
	})

	// The first run was invoked with an empty SessionID (fresh session).
	ids := fr.ids()
	if len(ids) == 0 || ids[0] != "" {
		t.Fatalf("first run SessionID = %q, want empty (fresh)", firstOrEmpty(ids))
	}

	// Second run for the same chat must resume: it is invoked with the stored id.
	svc.Handle(context.Background(), 100, 100, 2, "again")
	waitUntil(t, func() bool {
		ids := fr.ids()
		return len(ids) == 2 && ids[1] == "sess-xyz"
	})
}

// TestSessionPreservedOnTimeout asserts the §7.3 fix: when a run is cancelled/
// times out AFTER SystemInit but BEFORE the Result, the captured session_id is
// STILL stored, so the next message can resume — AC2.
func TestSessionPreservedOnTimeout(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	// gate is never closed; the run blocks after SystemInit until the per-run
	// timeout fires and cancels it.
	fr := &sessionRunner{emitInit: "sess-timeout", emitFinal: "sess-timeout", gate: make(chan struct{})}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 30*time.Millisecond)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "long task")

	// Despite the run timing out before any Result, the SystemInit session_id must
	// remain stored (NOT discarded/deleted).
	waitUntil(t, func() bool {
		id, ok := sess.get(100)
		return ok && id == "sess-timeout"
	})

	// And the user sees a terminal "Stopped" notice (timeout cancels delivery).
	waitUntil(t, func() bool {
		text, stop := fc.snapshot(1)
		return strings.Contains(text, "Stopped") && !stop
	})

	// A subsequent message resumes the preserved session.
	fr.gate = nil // let later runs complete normally
	svc.Handle(context.Background(), 100, 100, 2, "resume please")
	waitUntil(t, func() bool {
		ids := fr.ids()
		return len(ids) == 2 && ids[1] == "sess-timeout"
	})
}

// TestSessionPersistsWithRealStore exercises the Service against the real
// file-backed store and confirms the id survives a fresh Open of the same path
// (restart) — bridging AC1's store and AC2's adapter wiring.
func TestSessionPersistsWithRealStore(t *testing.T) {
	fc := newFakeChat()
	path := t.TempDir() + "/sessions.json"
	store, err := openRealStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	fr := &sessionRunner{emitInit: "sess-real", emitFinal: "sess-real"}
	svc, d := newTestServiceWithSessions(t, fr, fc, store, 0)
	defer d.Close()

	svc.Handle(context.Background(), 555, 555, 1, "hi")
	waitUntil(t, func() bool {
		id, ok := store.Get(555)
		return ok && id == "sess-real"
	})

	// Reopen the same path (simulating restart) and confirm the id is there.
	reopened, err := openRealStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if id, ok := reopened.Get(555); !ok || id != "sess-real" {
		t.Fatalf("after reopen Get = (%q, %v), want (%q, true)", id, ok, "sess-real")
	}
}

// TestSessionPreservedOnShutdown is the AC5 regression: when the dispatcher is
// shut down with a run in flight (after SystemInit, before Result), the captured
// session_id stays persisted so a restart resumes the conversation. This guards
// the graceful-shutdown path that storeSession-on-SystemInit (plan §7.3)
// underpins — a drain-cancel must NOT discard the id.
func TestSessionPreservedOnShutdown(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	// gate is never closed: the run blocks after SystemInit until Shutdown cancels
	// its context, mimicking a SIGTERM mid-run.
	fr := &sessionRunner{emitInit: "sess-shutdown", emitFinal: "sess-shutdown", gate: make(chan struct{})}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)

	svc.Handle(context.Background(), 100, 100, 1, "long task")

	// Wait until the run has emitted SystemInit and persisted the session id.
	waitUntil(t, func() bool {
		id, ok := sess.get(100)
		return ok && id == "sess-shutdown"
	})

	// Drain: cancel in-flight runs within a bounded budget (as main does on
	// SIGINT/SIGTERM). The run is gated, so it unwinds via ctx cancellation.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown drain: %v", err)
	}

	// The session id captured at SystemInit must survive the shutdown intact.
	if id, ok := sess.get(100); !ok || id != "sess-shutdown" {
		t.Fatalf("after shutdown Get = (%q, %v), want (%q, true)", id, ok, "sess-shutdown")
	}
	// And it was Set before the cancel delivered (storeSession-on-SystemInit), so
	// exactly the init id is recorded.
	if log := sess.sets(); len(log) == 0 || log[0] != "sess-shutdown" {
		t.Fatalf("setLog = %v, want first = sess-shutdown", log)
	}
}

// fixedOutbox resolves every chat to one fixed directory, for the finish-path
// sweep tests.
type fixedOutbox struct{ dir string }

func (f fixedOutbox) OutboxDir(int64) (string, error) { return f.dir, nil }

// newTestServiceWithOutbox builds a Service wired to a Sweeper over a fixed
// outbox dir (and an optional per-run timeout) for the AC2 finish-path tests.
func newTestServiceWithOutbox(t *testing.T, r claude.Runner, c chat, dir string, timeout time.Duration) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Chat:       c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Outbox:     NewSweeper(fixedOutbox{dir: dir}, 0, 0, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))),
		Timeout:    timeout,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// TestOutboxSweptOnNormalResult asserts that, on a normal Result, an outbox file
// is delivered as a document AND the text answer is still delivered (AC2).
func TestOutboxSweptOnNormalResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.Result, Result: &claude.RunResult{Text: "final answer"}},
	}}
	svc, d := newTestServiceWithOutbox(t, fr, fc, dir, 0)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "go")

	// The text answer reaches the progress message.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot(1)
		return text == "final answer"
	})
	// And the outbox file is delivered as a document and archived.
	waitUntil(t, func() bool {
		docs := fc.sentDocs()
		return len(docs) == 1 && docs[0] == "report.txt"
	})
	waitUntil(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "sent", "report.txt"))
		return err == nil
	})
}

// TestOutboxSweptOnStopCancel is the core AC2 guarantee: even when the run ctx is
// cancelled (Stop/timeout), files produced before the cancel are still swept,
// because finish uses deliverCtx (context.WithoutCancel).
func TestOutboxSweptOnStopCancel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	fc := newFakeChat()
	gate := make(chan struct{})
	fr := &fakeRunner{
		events: []claude.Event{{Type: claude.ToolUse, Tool: "Bash"}},
		gate:   gate, // run hangs until ctx is cancelled
	}
	svc, d := newTestServiceWithOutbox(t, fr, fc, dir, 0)
	defer d.Close()

	svc.Handle(context.Background(), 100, 100, 1, "go")
	waitUntil(t, func() bool { return svc.Stop("1") })

	// The terminal "Stopped" notice is delivered.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot(1)
		return strings.Contains(text, "Stopped")
	})
	// Despite the cancelled run ctx, the outbox file is still swept on deliverCtx.
	waitUntil(t, func() bool {
		docs := fc.sentDocs()
		return len(docs) == 1 && docs[0] == "partial.txt"
	})
	close(gate)
}

func firstOrEmpty(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// waitUntil polls cond up to a short deadline, failing the test on timeout.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
