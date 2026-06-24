//nolint:testpackage // intentionally whitebox to test unexported chat run loop internals
package chat

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

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/pending"
	"github.com/duckbugio/flock/core/session"
)

// Shared fixture values used across multiple run-loop tests.
const (
	finalAnswer  = "final answer"
	sessShutdown = "sess-shutdown"
	// anchorMsgID is the id of the first message the fake chat hands out, which is
	// the progress/answer anchor every single-run test inspects.
	anchorMsgID = "1"
	// testChatID is the chat id the shutdown tests submit under.
	testChatID = "100"
)

// openRealStore opens the real file-backed session store; it is a thin alias so
// the test body reads clearly.
func openRealStore(path string) (*session.FileStore, error) {
	return session.Open(path)
}

// fakeChat records the sequence of Telegram operations and lets a test inspect
// the final state. It is safe for concurrent use because edits arrive from the
// ticker goroutine and the run goroutine.
type fakeChat struct {
	mu      sync.Mutex
	nextID  int
	texts   map[MessageID]string // current text per message id
	hasStop map[MessageID]bool   // whether the message currently shows a Stop button
	deleted map[MessageID]bool
	sent    []string // every Send'd text, in order
	docs    []string // every SendDocument'd filename, in order
	nudges  []string // every SendStarNudge'd text, in order
	editErr error    // when set, Edit returns it (e.g. a 429 to exercise throttling)
	edits   int      // count of Edit calls (progress + final), for the throttle test
}

func newFakeChat() *fakeChat {
	return &fakeChat{
		texts:   map[MessageID]string{},
		hasStop: map[MessageID]bool{},
		deleted: map[MessageID]bool{},
	}
}

// Capabilities reports a full-capability transport so the fake drives the same
// run-loop path Telegram does (documents, 4096 cap).
func (f *fakeChat) Capabilities() Capabilities {
	return Capabilities{CanSendDocument: true, MaxMessageRunes: TelegramMaxMessage}
}

func (f *fakeChat) Send(_ context.Context, _ ChatID, text, stopRunID string, _ bool) (MessageID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	f.texts[id] = text
	f.hasStop[id] = stopRunID != ""
	f.sent = append(f.sent, text)
	return id, nil
}

func (f *fakeChat) Edit(_ context.Context, _ ChatID, messageID MessageID, text, stopRunID string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits++
	if f.editErr != nil {
		return f.editErr
	}
	f.texts[messageID] = text
	f.hasStop[messageID] = stopRunID != ""
	return nil
}

// editCount reports how many Edit calls the chat has received.
func (f *fakeChat) editCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.edits
}

func (f *fakeChat) Delete(_ context.Context, _ ChatID, messageID MessageID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[messageID] = true
	return nil
}

func (f *fakeChat) SendDocument(_ context.Context, _ ChatID, name string, data io.Reader) error {
	// Drain the reader so the fake behaves like a real upload (and so the source
	// file is read to completion before the sweeper closes it).
	_, _ = io.Copy(io.Discard, data)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, name)
	return nil
}

func (f *fakeChat) SendStarNudge(_ context.Context, _ ChatID, text string) (MessageID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	f.texts[id] = text
	f.nudges = append(f.nudges, text)
	return id, nil
}

func (f *fakeChat) sentDocs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.docs...)
}

func (f *fakeChat) sentNudges() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.nudges...)
}

func (f *fakeChat) snapshot() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.texts[anchorMsgID], f.hasStop[anchorMsgID]
}

// fakeWorkspace returns a fixed path and records the chat ids it was asked for.
type fakeWorkspace struct {
	mu    sync.Mutex
	calls []ChatID
	err   error
}

func (w *fakeWorkspace) Ensure(chatID ChatID) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, chatID)
	if w.err != nil {
		return "", w.err
	}
	return "/tmp/chat_" + chatID, nil
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
	store  map[ChatID]string
	setLog []string // sessionIDs passed to Set, in order
	delLog []ChatID // chatIDs passed to Delete, in order
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{store: map[ChatID]string{}}
}

func (f *fakeSessions) Get(chatID ChatID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.store[chatID]
	return id, ok
}

func (f *fakeSessions) Set(chatID ChatID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID == "" {
		return nil
	}
	f.store[chatID] = sessionID
	f.setLog = append(f.setLog, sessionID)
	return nil
}

func (f *fakeSessions) Delete(chatID ChatID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, chatID)
	f.delLog = append(f.delLog, chatID)
	return nil
}

func (f *fakeSessions) get() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.store[testChatID]
	return id, ok
}

func (f *fakeSessions) deletes() []ChatID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ChatID(nil), f.delLog...)
}

func (f *fakeSessions) sets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setLog...)
}

// fakePending is an in-memory pendingStore for the marker tests, modelling the
// real per-chat FIFO queue + by-id clearing. It records the current queues plus a
// log of mutating calls so a test can assert what was written and when.
type fakePending struct {
	mu      sync.Mutex
	store   map[ChatID][]pending.Marker
	nextID  int
	enqLog  []ChatID // chat ids that had a marker enqueued, in order
	remLog  []ChatID // chat ids whose marker was Remove'd (by id), in order
	clearLg []ChatID // chat ids whose whole lane was Clear'd, in order
}

func newFakePending() *fakePending {
	return &fakePending{store: map[ChatID][]pending.Marker{}, nextID: 1}
}

func (f *fakePending) Enqueue(chatID ChatID, marker pending.Marker) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strconv.Itoa(f.nextID)
	f.nextID++
	marker.ID = id
	f.store[chatID] = append(f.store[chatID], marker)
	f.enqLog = append(f.enqLog, chatID)
	return id, nil
}

func (f *fakePending) SetAnchor(chatID ChatID, id, anchorMsgID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.store[chatID] {
		if f.store[chatID][i].ID == id {
			f.store[chatID][i].AnchorMsgID = anchorMsgID
			return nil
		}
	}
	return nil
}

func (f *fakePending) Remove(chatID ChatID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.store[chatID]
	for i := range q {
		if q[i].ID == id {
			q = append(q[:i], q[i+1:]...)
			if len(q) == 0 {
				delete(f.store, chatID)
			} else {
				f.store[chatID] = q
			}
			f.remLog = append(f.remLog, chatID)
			return nil
		}
	}
	return nil
}

func (f *fakePending) Clear(chatID ChatID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, chatID)
	f.clearLg = append(f.clearLg, chatID)
	return nil
}

func (f *fakePending) All() map[ChatID][]pending.Marker {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[ChatID][]pending.Marker{}
	for k, v := range f.store {
		out[k] = append([]pending.Marker(nil), v...)
	}
	return out
}

// count reports how many markers are queued for testChatID (the id every marker
// test submits under).
func (f *fakePending) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.store[testChatID])
}

// has reports whether any marker is stored for testChatID (the id every marker
// test submits under), mirroring fakeSessions.get's param-free style.
func (f *fakePending) has() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.store[testChatID]) > 0
}

// get returns the first queued marker for testChatID.
func (f *fakePending) get() (pending.Marker, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.store[testChatID]
	if len(q) == 0 {
		return pending.Marker{}, false
	}
	return q[0], true
}

// removes returns the chat ids whose marker was cleared by id (Remove) or whose
// lane was cleared (Clear), in call order — every path that drops a marker.
func (f *fakePending) removes() []ChatID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]ChatID(nil), f.remLog...)
	return append(out, f.clearLg...)
}

// newTestServiceWithPending builds a Service wired to a pending store (and a fake
// session store) over a real Dispatcher, for the interrupted-run marker tests.
func newTestServiceWithPending(
	t *testing.T, r claude.Runner, c Transport, p pendingStore,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Pending:    p,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// newTestService builds a Service over a real Dispatcher and a fake workspace.
func newTestService(t *testing.T, r claude.Runner, c Transport) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond // speed up the wall-clock ticker for tests
	return s, d
}

// newTestServiceWithSessions builds a Service wired to a session store (and an
// optional per-run timeout) for the Stage 5 continuity/timeout tests.
func newTestServiceWithSessions(
	t *testing.T, r claude.Runner, c Transport, sess sessionStore, timeout time.Duration,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Sessions:   sess,
		Timeout:    timeout,
		Logger:     slog.New(slog.DiscardHandler),
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
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer, SessionID: "s1"}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), "100", 100, "1", "hello")

	// The progress message (id 1) should eventually hold the final answer with no
	// Stop button.
	waitUntil(t, func() bool {
		text, stop := fc.snapshot()
		return text == finalAnswer && !stop
	})
}

// TestRunDeliversSingleProgressBubble asserts a run is exactly ONE chat bubble: the
// anchor (carrying the Stop button) is edited in place with progress and then with
// the final answer, and NO draft preview is ever streamed — so a 1:1 chat no longer
// shows a second "Working…" bubble beside the anchor.
func TestRunDeliversSingleProgressBubble(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.ToolUse, Tool: "Bash"},
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer, SessionID: "s1"}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), "100", 100, "1", "hello")

	waitUntil(t, func() bool {
		text, stop := fc.snapshot()
		return text == finalAnswer && !stop
	})

	fc.mu.Lock()
	defer fc.mu.Unlock()
	// The anchor was sent as the fixed Stop-bearing anchor text; it is then edited in
	// place (progress, then the answer) — one persistent message for the whole run.
	if len(fc.sent) == 0 || fc.sent[0] != anchorText {
		t.Fatalf("anchor not sent as the fixed anchor text %q; sent=%v", anchorText, fc.sent)
	}
	// The answer replaced the anchor via an Edit (waitUntil already saw the anchor
	// text become the answer with the Stop button cleared), not a fresh Send.
	if fc.edits == 0 {
		t.Fatalf("answer not delivered by editing the anchor; edits=%d", fc.edits)
	}
}

// manualClock is a thread-safe, manually-advanced clock for nowFunc injection.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock { return &manualClock{t: time.Unix(0, 0)} }

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestProgressEditsRespectMinInterval drives the rate-limited progress edits with
// a fast ticker while advancing the injected clock one minEditInterval window at a
// time, and asserts that real edits are bounded to roughly one per minEditInterval
// window (not one per tick) AND that the rendered counter still advances across
// windows — i.e. the throttle bounds edits without freezing the counter.
//
// It is deterministic by construction: after every clock advance that should permit
// a new edit it polls (via waitUntil) until that edit has actually landed — the
// counter reaches the value implied by the new clock — before advancing again. It
// never sleeps a fixed wall-clock window "hoping" a real tick fired, so it cannot go
// red under -race when the ticker goroutine is starved by CPU contention; a starved
// tick only makes waitUntil poll a little longer.
func TestProgressEditsRespectMinInterval(t *testing.T) {
	fc := newFakeChat()
	gate := make(chan struct{})
	fr := &fakeRunner{
		events: []claude.Event{{Type: claude.ToolUse, Tool: "Bash"}},
		gate:   gate, // keep the run in flight while we drive ticks/clock
	}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	clk := newManualClock()
	svc.nowFunc = clk.now
	svc.tick = time.Millisecond // tick fast so a tick lands quickly once an edit is due

	svc.Handle(context.Background(), "100", 100, "1", "go")

	// Wait until the anchor exists (the run is up and in fallback mode).
	waitUntil(t, func() bool {
		_, stop := fc.snapshot()
		return stop
	})

	// Walk the clock forward one full minEditInterval window per step. The run's
	// elapsed counter is nowFunc()-start with start captured at clock 0, so after
	// window i the counter reads i*minEditInterval seconds. Advancing by exactly
	// minEditInterval puts the next tick at the throttle boundary (Sub >=
	// minEditInterval), so exactly one new edit becomes due per window; lastEditAt is
	// zero before the first window, so the first window's edit fires immediately too.
	const windows = 4
	intervalSecs := int(minEditInterval / time.Second)
	for w := 1; w <= windows; w++ {
		clk.advance(minEditInterval)
		// Poll until this window's edit has actually landed: the counter reaches
		// w*intervalSecs seconds. waitUntil makes this deterministic regardless of when
		// the real ticker goroutine gets scheduled — no fixed sleep, no race.
		wantSecs := w * intervalSecs
		// A generous deadline: the millisecond ticker that fires render() runs in a
		// background goroutine that can be starved for seconds under `go test -race
		// ./...` (all packages in parallel saturating GOMAXPROCS). The wait is still
		// deterministic — the clock is already advanced, so the next tick that runs
		// renders exactly (wantSecs)s — it just may take a while to be scheduled.
		waitUntilFor(t, 30*time.Second, func() bool {
			text, _ := fc.snapshot()
			return strings.Contains(text, "Working") &&
				strings.Contains(text, "("+strconv.Itoa(wantSecs)+"s)")
		})
	}

	// Edits during the run are throttled to ~one per window: with `windows` windows
	// (plus at most the prompt first-edit) we expect far fewer than the thousands of
	// ticks the millisecond ticker fired across this span.
	progressEdits := fc.editCount()
	if progressEdits > windows+2 {
		t.Fatalf("edits not throttled: %d edits across %d windows", progressEdits, windows)
	}
	if progressEdits == 0 {
		t.Fatal("no edits happened in fallback mode; counter would be frozen")
	}

	// The counter advanced across every window without the throttle freezing render():
	// the final per-window waitUntil above already confirmed it reached the last
	// window's value, so the rendered counter ends at windows*minEditInterval seconds.
	wantFinalSecs := windows * intervalSecs
	text, _ := fc.snapshot()
	if !strings.Contains(text, "("+strconv.Itoa(wantFinalSecs)+"s)") {
		t.Fatalf("final counter = %q, want it to contain (%ds)", text, wantFinalSecs)
	}

	close(gate)
}

func TestRunChunksLongFinal(t *testing.T) {
	long := strings.Repeat("x", TelegramMaxMessage+500)
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.Result, Result: &claude.RunResult{Text: long}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()
	svc.Handle(context.Background(), "100", 100, "1", "go")

	// Wait until the run completed: the anchor send, the tail chunk send, and the
	// separate completion notice (the final Send) have all landed.
	waitUntil(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return len(fc.sent) == 3
	})

	// First chunk replaces the progress message; the remainder is a new Send.
	first, _ := fc.snapshot()
	if utf8.RuneCountInString(first) > TelegramMaxMessage {
		t.Fatalf("first chunk too big: %d", utf8.RuneCountInString(first))
	}
	// The chunk tail is a real Send (not the anchor, not the notice), so a long
	// answer still spans multiple messages.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.sent[0] != anchorText {
		t.Fatalf("first send was not the anchor: %q", fc.sent[0])
	}
}

// noticeCount reports how many of the chat's Send'd texts contain sub (used to
// assert the completion notice is a real Send that fires exactly once).
func (f *fakeChat) noticeCount(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// TestCompletionNoticeOnResult (AC3) asserts a normal Result delivers a SEPARATE
// completion "Done" notice (a real Send, not only the in-place Edit) and that it
// fires exactly once, distinct from the edited answer bubble.
func TestCompletionNoticeOnResult(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer}},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), "100", 100, "1", "go")

	// The answer reaches the anchor via Edit.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return text == finalAnswer
	})
	// A separate "Done" notice is Send'd exactly once.
	waitUntil(t, func() bool { return fc.noticeCount("✅ Done in") == 1 })
	if n := fc.noticeCount("✅ Done in"); n != 1 {
		t.Fatalf("completion notice sent %d times, want exactly 1", n)
	}
	// It is a genuine Send distinct from the edited answer (the anchor still holds the
	// answer, not the notice).
	text, _ := fc.snapshot()
	if strings.Contains(text, "✅ Done in") {
		t.Fatalf("notice was attached to the live edit, not a separate Send: %q", text)
	}
}

// TestCompletionNoticeFormatsTotal (AC3) drives the run with a manual clock so the
// total elapsed is deterministic, and asserts the notice carries the humanized
// duration from formatElapsed.
func TestCompletionNoticeFormatsTotal(t *testing.T) {
	fc := newFakeChat()
	gate := make(chan struct{})
	fr := &fakeRunner{
		events: []claude.Event{{Type: claude.ToolUse, Tool: "Bash"}},
		gate:   gate,
	}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	clk := newManualClock()
	svc.nowFunc = clk.now
	svc.Handle(context.Background(), "100", 100, "1", "go")

	// Wait until the run is up (anchor with Stop posted), so start has been captured
	// at clock 0; then advance the clock and release the run. The terminal funnel reads
	// now - start = the advanced amount.
	waitUntil(t, func() bool {
		_, stop := fc.snapshot()
		return stop
	})
	clk.advance(125 * time.Second) // 2m 05s
	close(gate)

	want := "✅ Done in " + formatElapsed(125)
	waitUntil(t, func() bool { return fc.noticeCount(want) == 1 })
	if n := fc.noticeCount(want); n != 1 {
		t.Fatalf("notice with total %q sent %d times, want exactly 1; sent=%v", want, n, fc.sent)
	}
}

// TestCompletionNoticeOnRunError (AC3) asserts a RunError terminal delivers a
// "Failed" completion notice as a separate Send.
func TestCompletionNoticeOnRunError(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.RunError, Err: errors.New("boom")},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), "100", 100, "1", "go")

	waitUntil(t, func() bool { return fc.noticeCount("⚠️ Failed after") == 1 })
	if n := fc.noticeCount("⚠️ Failed after"); n != 1 {
		t.Fatalf("RunError notice sent %d times, want exactly 1", n)
	}
}

// TestCompletionNoticeOnStop (AC3) asserts a user Stop (ctx cancelled, no Result)
// delivers a "Stopped" completion notice as a separate Send, fired once.
func TestCompletionNoticeOnStop(t *testing.T) {
	fc := newFakeChat()
	gate := make(chan struct{})
	fr := &fakeRunner{
		events: []claude.Event{{Type: claude.ToolUse, Tool: "Bash"}},
		gate:   gate,
	}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()

	svc.Handle(context.Background(), "100", 100, "1", "go")
	waitUntil(t, func() bool { return svc.Stop("1") })

	waitUntil(t, func() bool { return fc.noticeCount("⏹ Stopped after") == 1 })
	if n := fc.noticeCount("⏹ Stopped after"); n != 1 {
		t.Fatalf("Stop notice sent %d times, want exactly 1", n)
	}
	close(gate)
}

// TestCompletionNoticeStatusWords pins the status word + total for every terminal
// case, including the deploy-shutdown "resuming" case that must read "paused …
// will resume" (not "stopped"), since such a run keeps its marker and auto-resumes.
func TestCompletionNoticeStatusWords(t *testing.T) {
	const total = 4*time.Minute + 12*time.Second
	tests := []struct {
		name     string
		res      *claude.RunResult
		runErr   error
		ctxErr   error
		resuming bool
		want     string
	}{
		{"clean result", &claude.RunResult{}, nil, nil, false, "✅ Done in 4m 12s"},
		{"run error", nil, errors.New("boom"), nil, false, "⚠️ Failed after 4m 12s"},
		{"is_error result", &claude.RunResult{IsError: true}, nil, nil, false, "⚠️ Failed after 4m 12s"},
		{"user stop", nil, nil, context.Canceled, false, "⏹ Stopped after 4m 12s"},
		{"deploy shutdown resumes", nil, nil, context.Canceled, true, "⏳ Paused after 4m 12s — will resume after restart"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := completionNotice(tc.res, tc.runErr, tc.ctxErr, total, tc.resuming); got != tc.want {
				t.Errorf("completionNotice = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunErrorEvent(t *testing.T) {
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.RunError, Err: errors.New("boom")},
	}}
	svc, d := newTestService(t, fr, fc)
	defer d.Close()
	svc.Handle(context.Background(), "100", 100, "1", "go")

	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
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

	svc.Handle(context.Background(), "100", 100, "1", "go")

	// Wait until the run is registered, then Stop it. runID for the first run is "1".
	waitUntil(t, func() bool { return svc.Stop("1") })

	waitUntil(t, func() bool {
		text, stop := fc.snapshot()
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
		Transport:  fc,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{err: errors.New("no workspace")},
		Logger:     slog.New(slog.DiscardHandler),
	})
	svc.tick = 5 * time.Millisecond

	svc.Handle(context.Background(), "100", 100, "1", "go")
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
	svc.Handle(context.Background(), "100", 100, "1", "first")
	waitUntil(t, func() bool {
		_, stop := fc.snapshot()
		return stop // first run posted its progress message
	})

	svc.Handle(context.Background(), "100", 100, "2", "second")
	// The second run must not have started while the first is gated: no second
	// progress message (message id 2) should exist yet.
	time.Sleep(30 * time.Millisecond)
	fc.mu.Lock()
	_, secondStarted := fc.texts["2"]
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
	svc.Handle(context.Background(), "100", 100, "7", "first version")
	waitUntil(t, func() bool {
		return len(rr.seen()) == 1 && rr.seen()[0] == "first version"
	})

	// An edit of the SAME message id supersedes: cancel the in-flight run and
	// resubmit with the new text.
	svc.HandleEdit(context.Background(), "100", 100, "7", "edited version")

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
	svc.Handle(context.Background(), "100", 100, "20", "newer version")
	waitUntil(t, func() bool {
		return len(rr.seen()) == 1 && rr.seen()[0] == "newer version"
	})

	// An edit of an OLDER message (id 5) arrives. It is treated as a fresh run
	// (not a supersede) and must NOT rewind lastMsg back to 5. The fresh run is
	// queued behind the still-hanging newer run (serial per chat), so we assert on
	// lastMsg directly rather than waiting for the queued run to start.
	svc.HandleEdit(context.Background(), "100", 100, "5", "older edit")

	svc.mu.Lock()
	last := svc.lastMsg["100"]
	svc.mu.Unlock()
	if last != "20" {
		t.Fatalf("lastMsg rewound to %q, want 20 (newer message must stay latest)", last)
	}

	// Now editing the newer message (id 20) must still supersede its in-flight
	// run: because lastMsg was not rewound, this matches the latest and cancels
	// the in-flight run. With lastMsg left at 5 (the bug) this edit would instead
	// queue a duplicate run.
	svc.HandleEdit(context.Background(), "100", 100, "20", "newer edited")

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
	svc.HandleEdit(context.Background(), "200", 200, "99", "out of nowhere")

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
	svc.Handle(context.Background(), "100", 100, "1", "hello")
	waitUntil(t, func() bool {
		id, ok := sess.get()
		return ok && id == "sess-xyz"
	})

	// The first run was invoked with an empty SessionID (fresh session).
	ids := fr.ids()
	if len(ids) == 0 || ids[0] != "" {
		t.Fatalf("first run SessionID = %q, want empty (fresh)", firstOrEmpty(ids))
	}

	// Second run for the same chat must resume: it is invoked with the stored id.
	svc.Handle(context.Background(), "100", 100, "2", "again")
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

	svc.Handle(context.Background(), "100", 100, "1", "long task")

	// Despite the run timing out before any Result, the SystemInit session_id must
	// remain stored (NOT discarded/deleted).
	waitUntil(t, func() bool {
		id, ok := sess.get()
		return ok && id == "sess-timeout"
	})

	// And the user sees a terminal "Stopped" notice (timeout cancels delivery).
	waitUntil(t, func() bool {
		text, stop := fc.snapshot()
		return strings.Contains(text, "Stopped") && !stop
	})

	// A subsequent message resumes the preserved session.
	fr.gate = nil // let later runs complete normally
	svc.Handle(context.Background(), "100", 100, "2", "resume please")
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

	svc.Handle(context.Background(), "555", 555, "1", "hi")
	waitUntil(t, func() bool {
		id, ok := store.Get("555")
		return ok && id == "sess-real"
	})

	// Reopen the same path (simulating restart) and confirm the id is there.
	reopened, err := openRealStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if id, ok := reopened.Get("555"); !ok || id != "sess-real" {
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
	fr := &sessionRunner{emitInit: sessShutdown, emitFinal: sessShutdown, gate: make(chan struct{})}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)

	svc.Handle(context.Background(), "100", 100, "1", "long task")

	// Wait until the run has emitted SystemInit and persisted the session id.
	waitUntil(t, func() bool {
		id, ok := sess.get()
		return ok && id == sessShutdown
	})

	// Drain: WAIT for in-flight runs up to a bounded budget, then cancel the
	// survivors (as main does on SIGINT/SIGTERM). The run is gated and never
	// finishes on its own, so it survives the (short) window and is cancelled at the
	// deadline — Shutdown then reports the deadline. We assert the session id
	// survived that cancel, which is the point of this regression.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(shutdownCtx); err == nil {
		t.Fatal("Shutdown returned nil; the gated run should survive the window and be cancelled at the deadline")
	}

	// The session id captured at SystemInit must survive the shutdown intact.
	if id, ok := sess.get(); !ok || id != sessShutdown {
		t.Fatalf("after shutdown Get = (%q, %v), want (%q, true)", id, ok, sessShutdown)
	}
	// And it was Set before the cancel delivered (storeSession-on-SystemInit), so
	// exactly the init id is recorded.
	if log := sess.sets(); len(log) == 0 || log[0] != sessShutdown {
		t.Fatalf("setLog = %v, want first = sess-shutdown", log)
	}
}

// TestPendingMarkerSetAtSubmit asserts the interrupted-run marker is enqueued at
// SUBMIT (so a queued run is captured too), carries the prompt and a process id,
// gains the anchor message id once the anchor is sent, and is present while the
// run is still in flight.
func TestPendingMarkerSetAtSubmit(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// gate is never closed: the run blocks after SystemInit so we can inspect the
	// marker mid-run.
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "resume me")

	// The marker must appear with the prompt and the anchor message id while the run
	// is still in flight (before any terminal).
	waitUntil(t, func() bool {
		m, ok := pend.get()
		return ok && m.Prompt == "resume me" && m.AnchorMsgID == anchorMsgID && m.StartedAt != 0
	})
}

// TestPendingMarkerClearedOnNormalFinish asserts a normally-finished run clears its
// marker, so it is NOT auto-resumed after a later restart.
func TestPendingMarkerClearedOnNormalFinish(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer, SessionID: "s1"}},
	}}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	waitUntil(t, func() bool {
		text, stop := fc.snapshot()
		return text == finalAnswer && !stop
	})
	waitUntil(t, func() bool { return !pend.has() })
	if dels := pend.removes(); len(dels) == 0 || dels[len(dels)-1] != testChatID {
		t.Fatalf("marker not cleared on normal finish; removes=%v", dels)
	}
}

// TestPendingMarkerClearedOnStop asserts a user-Stopped run clears its marker (the
// terminal is a per-chat cancel, not a shutdown), so a deliberately stopped run is
// NOT auto-resumed.
func TestPendingMarkerClearedOnStop(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// gate never closes: the run blocks until Stop cancels its context.
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "stop me")

	// Wait until the run is in flight (marker present), then Stop it.
	waitUntil(t, func() bool { return pend.has() })
	waitUntil(t, func() bool { return svc.Stop("1") })

	// The Stop terminal must clear the marker.
	waitUntil(t, func() bool { return !pend.has() })
}

// TestPendingMarkerSurvivesShutdown asserts a run cancelled by the dispatcher
// SHUTTING DOWN (drain-deadline cancel) keeps its marker so the next startup can
// auto-resume it — the one terminal that must NOT clear the marker.
func TestPendingMarkerSurvivesShutdown(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// gate never closes: the run blocks until Shutdown cancels its context at the
	// (short) drain deadline.
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)

	svc.Handle(context.Background(), testChatID, 100, "1", "resume me")
	waitUntil(t, func() bool { return pend.has() })

	// Short drain window: the gated run never finishes, so Shutdown cancels it with
	// the ErrShutdown cause after the deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = d.Shutdown(shutdownCtx)

	// The marker must SURVIVE the shutdown so the run is auto-resumed on restart.
	if !pend.has() {
		t.Fatalf("marker was cleared on shutdown; removes=%v — a killed run must stay resumable", pend.removes())
	}
}

// newTestServiceWithPendingTimeout builds a Service wired to a pending store with
// a per-run timeout, for the timeout marker-clear test. It mirrors
// newTestServiceWithPending but injects a Timeout so a gated run hits the per-run
// deadline (cause context.DeadlineExceeded, NOT dispatch.ErrShutdown).
func newTestServiceWithPendingTimeout(
	t *testing.T, r claude.Runner, c Transport, p pendingStore, timeout time.Duration,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Pending:    p,
		Timeout:    timeout,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// TestPendingMarkerClearedOnTimeout asserts a run that hits its per-run timeout
// (ctx deadline expires for a non-shutdown reason — cause
// context.DeadlineExceeded, NOT dispatch.ErrShutdown) reaches finish and clears
// its marker, so a timed-out run is NOT auto-resumed. This is the symmetric
// counterpart to the normal-finish, Stop, and spawn-error clear tests, and the
// inverse of TestPendingMarkerSurvivesShutdown (the one terminal that preserves it).
func TestPendingMarkerClearedOnTimeout(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// gate never closes: the run blocks after SystemInit until the per-run timeout
	// fires and cancels it with cause context.DeadlineExceeded.
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPendingTimeout(t, fr, fc, pend, 30*time.Millisecond)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "long task")

	// The marker is enqueued at submit while the run is in flight.
	waitUntil(t, func() bool { return pend.has() })

	// The per-run timeout cancels the run (non-shutdown cause), the user sees the
	// terminal "Stopped" notice...
	waitUntil(t, func() bool {
		text, stop := fc.snapshot()
		return strings.Contains(text, "Stopped") && !stop
	})

	// ...and the timeout terminal clears the marker (cause is DeadlineExceeded, not
	// ErrShutdown), so the run is not auto-resumed.
	waitUntil(t, func() bool { return !pend.has() })
	if dels := pend.removes(); len(dels) == 0 || dels[len(dels)-1] != testChatID {
		t.Fatalf("marker not cleared on timeout; removes=%v", dels)
	}
}

// spawnErrorRunner fails to start: Run returns a non-nil error and never emits
// any events, modelling the CLI failing to spawn. When block is non-nil Run
// waits on it (or ctx.Done) before returning the error, letting a test cancel
// the run's context (e.g. via Shutdown) first so the failure is observed under a
// shutdown cause.
type spawnErrorRunner struct {
	err   error
	block chan struct{}
}

func (r *spawnErrorRunner) Run(ctx context.Context, _ string, _ claude.Options) (<-chan claude.Event, error) {
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
		}
	}
	return nil, r.err
}

// TestPendingMarkerClearedOnSpawnError asserts a run whose CLI fails to spawn
// (runner.Run returns an error) clears its marker — a clean, non-shutdown
// terminal that returns before finish, so it must NOT be auto-resumed. A
// persistent spawn error would otherwise re-resume and re-leak every boot.
func TestPendingMarkerClearedOnSpawnError(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	fr := &spawnErrorRunner{err: errors.New("spawn failed")}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	// The spawn-error terminal must clear the marker enqueued at submit. The marker
	// is enqueued and cleared around the same dispatched run, so assert on the
	// remove log (the transient marker may never be observable mid-flight).
	waitUntil(t, func() bool {
		dels := pend.removes()
		return len(dels) > 0 && dels[len(dels)-1] == testChatID
	})
	if pend.has() {
		t.Fatalf("marker still present after spawn error; it must be cleared")
	}
}

// TestPendingMarkerSurvivesSpawnErrorOnShutdown asserts a spawn failure caused by
// the dispatcher SHUTTING DOWN keeps its marker so the next startup auto-resumes
// it — the spawn-error clear is gated exactly like finish on the shutdown cause.
func TestPendingMarkerSurvivesSpawnErrorOnShutdown(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// block never closes: Run waits until Shutdown cancels its context (cause
	// ErrShutdown), then returns the error — a shutdown-caused start failure.
	fr := &spawnErrorRunner{err: errors.New("spawn failed"), block: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)

	svc.Handle(context.Background(), testChatID, 100, "1", "resume me")
	waitUntil(t, func() bool { return pend.has() })

	// Short drain window: the gated Run never returns until cancelled, so Shutdown
	// cancels it with the ErrShutdown cause after the deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = d.Shutdown(shutdownCtx)

	// The marker must SURVIVE so the run is auto-resumed on restart.
	if !pend.has() {
		t.Fatalf("marker cleared on shutdown-caused spawn error; removes=%v — a killed run must stay resumable", pend.removes())
	}
}

// TestPendingQueuedAndRunningBothSurviveShutdown is the finding-#1 regression: a
// chat with one RUNNING run (A) and one QUEUED-but-never-started run (B) on the
// same serial lane must keep BOTH markers when the process is shut down, so BOTH
// resume on restart. The old per-chat single marker could hold only one of them
// and a queued job never reached run-start to mark itself, silently losing B.
func TestPendingQueuedAndRunningBothSurviveShutdown(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// gate never closes: A blocks after SystemInit (in flight), so B stays queued
	// behind it on the serial per-chat lane and never starts.
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)

	// Two submissions on the SAME chat: A runs (and blocks), B queues behind it.
	svc.Handle(context.Background(), testChatID, 100, "1", "run A")
	svc.Handle(context.Background(), testChatID, 100, "2", "queue B")

	// Both markers must be enqueued at submit (A in flight, B still queued).
	waitUntil(t, func() bool { return pend.count() == 2 })

	// Short drain window: the gated A never finishes (cause ErrShutdown), B never
	// starts, so neither reaches a clean terminal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = d.Shutdown(shutdownCtx)

	// BOTH markers must survive so BOTH runs auto-resume on restart.
	if got := pend.count(); got != 2 {
		t.Fatalf("after shutdown queue len = %d, want 2 (both queued+running must survive); removes=%v", got, pend.removes())
	}
	prompts := map[string]bool{}
	for _, m := range pend.All()[testChatID] {
		prompts[m.Prompt] = true
	}
	if !prompts["run A"] || !prompts["queue B"] {
		t.Fatalf("surviving markers = %v, want both 'run A' and 'queue B'", prompts)
	}
}

// resultThenBlockRunner emits SystemInit + a clean Result, signals delivered once
// the Result has been consumed by the event loop, then BLOCKS on ctx.Done before
// closing its event channel. This lets a test deterministically order: the run
// reaches a clean terminal (Result delivered), and ONLY THEN is its ctx cancelled
// with the ErrShutdown cause — modelling the microsecond window at the drain
// deadline where an already-finished run is shutdown-cancelled.
type resultThenBlockRunner struct {
	delivered chan struct{} // closed after the Result has been consumed
}

func (r *resultThenBlockRunner) Run(ctx context.Context, _ string, _ claude.Options) (<-chan claude.Event, error) {
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		if !emit(ctx, out, claude.Event{Type: claude.SystemInit, SessionID: "s1"}) {
			return
		}
		if !emit(ctx, out, claude.Event{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer, SessionID: "s1"}}) {
			return
		}
		// The Result is now in the event loop's hands (unbuffered send returned):
		// finalResult is set. Signal the test, then block until the ctx is cancelled
		// so finish runs AFTER the (shutdown) cancel — but with a non-nil result.
		close(r.delivered)
		<-ctx.Done()
	}()
	return out, nil
}

// TestPendingMarkerClearedOnResultEvenUnderShutdown is the phantom-resume
// regression: a run that REACHED a clean Result and is THEN shutdown-cancelled
// (cause dispatch.ErrShutdown) in the microsecond window at the drain deadline
// must still CLEAR its marker — it already produced a terminal, so resuming it on
// the next boot would be a duplicate run (re-triggering side effects). The marker
// is kept ONLY for a run that produced nothing under shutdown (covered by
// TestPendingMarkerSurvivesShutdown), not for one that finished.
func TestPendingMarkerClearedOnResultEvenUnderShutdown(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	fr := &resultThenBlockRunner{delivered: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)

	svc.Handle(context.Background(), testChatID, 100, "1", "almost done")

	// Wait until the run has delivered its clean Result (finalResult is set) and is
	// blocked on ctx.Done — finish has not run yet.
	select {
	case <-fr.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("run never delivered its Result")
	}

	// Now shut down: the blocked run's ctx is cancelled with the ErrShutdown cause,
	// it returns, the channel closes, the loop breaks, and finish runs with a
	// non-nil result under a shutdown cause.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Shutdown(shutdownCtx)

	// The marker MUST be cleared: the run reached a terminal, so it is not a
	// genuine mid-flight interruption and must never be phantom-resumed.
	waitUntil(t, func() bool { return !pend.has() })
	if dels := pend.removes(); len(dels) == 0 || dels[len(dels)-1] != testChatID {
		t.Fatalf("marker not cleared on result under shutdown; removes=%v — finished run would be phantom-resumed", dels)
	}
}

// TestPendingSupersedeDoesNotLoseSuccessor asserts an edit-supersede (Cancel +
// clear-lane + enqueue the new run) leaves exactly the NEW run's marker, and the
// cancelled run's by-id clear cannot remove that successor. This is the
// concurrency property: clear-by-id means a stale run never drops a newer marker.
func TestPendingSupersedeDoesNotLoseSuccessor(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	// The first run blocks after SystemInit so it is genuinely in flight when the
	// edit supersedes it.
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	// Original message (msg id 1) starts a run and is recorded as the chat's latest.
	svc.Handle(context.Background(), testChatID, 100, "1", "original")
	waitUntil(t, func() bool { return pend.has() })

	// Edit that same message: supersede cancels the in-flight run, clears the lane,
	// and enqueues the new run.
	svc.HandleEdit(context.Background(), testChatID, 100, "1", "edited")

	// The new run's marker must be the one that survives; the cancelled run's by-id
	// clear must not remove it. Eventually the queue holds exactly the edited run.
	waitUntil(t, func() bool {
		q := pend.All()[testChatID]
		return len(q) == 1 && q[0].Prompt == "edited"
	})
}

// TestPendingStopClearsLane asserts /stop (via svc.Stop) purges the WHOLE lane's
// markers — both the running run and any queued successors — so nothing the user
// deliberately stopped is auto-resumed.
func TestPendingStopClearsLane(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	fr := &sessionRunner{emitInit: "s1", emitFinal: "s1", gate: make(chan struct{})}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	// A runs (blocks), B queues behind it — two markers on the lane.
	svc.Handle(context.Background(), testChatID, 100, "1", "run A")
	svc.Handle(context.Background(), testChatID, 100, "2", "queue B")
	waitUntil(t, func() bool { return pend.count() == 2 })

	// Stop the running run: it cancels the lane AND clears every marker.
	waitUntil(t, func() bool { return svc.Stop("1") })

	waitUntil(t, func() bool { return pend.count() == 0 })
}

// TestResumePendingReusesIDNoDuplicate asserts ResumePending replays a marker by
// REUSING its existing id (not enqueuing a fresh one), so the resumed run clears
// the SAME on-disk marker on its clean terminal and never leaves a duplicate. It
// mirrors the startup replay: the store already holds the persisted markers.
func TestResumePendingReusesIDNoDuplicate(t *testing.T) {
	fc := newFakeChat()
	pend := newFakePending()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.SystemInit, SessionID: "s1"},
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer, SessionID: "s1"}},
	}}
	svc, d := newTestServiceWithPending(t, fr, fc, pend)
	defer d.Close()

	// Seed two persisted markers (as if left behind by a deploy), in order.
	idA, _ := pend.Enqueue(testChatID, pending.Marker{Prompt: "A", StartedAt: 1})
	idB, _ := pend.Enqueue(testChatID, pending.Marker{Prompt: "B", StartedAt: 2})

	// Replay both in order, exactly as resumePending does — reusing their ids.
	for _, m := range pend.All()[testChatID] {
		svc.ResumePending(testChatID, m)
	}

	// Each resumed run clears its OWN marker by id on its clean terminal; no fresh
	// marker is enqueued, so the lane drains to empty (no duplicate left behind).
	waitUntil(t, func() bool { return pend.count() == 0 })

	// The runs reused the seeded ids — no fresh ids were minted at replay (Enqueue
	// was called exactly twice, by the seed above).
	if got := len(pend.enqLog); got != 2 {
		t.Fatalf("Enqueue called %d times, want 2 (replay must reuse ids, not enqueue)", got)
	}
	if idA == idB {
		t.Fatalf("seeded ids collided: %q", idA)
	}
}

// errorResultRunner emits a SystemInit (so the session id is captured) followed
// by a terminal Result whose IsError flag is configurable. It lets the
// stale-session self-heal tests drive an is_error vs a clean Result. When init is
// empty no SystemInit is emitted (a fresh run with no captured session id).
type errorResultRunner struct {
	init    string
	isError bool
}

func (r *errorResultRunner) Run(ctx context.Context, _ string, _ claude.Options) (<-chan claude.Event, error) {
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		if r.init != "" {
			if !emit(ctx, out, claude.Event{Type: claude.SystemInit, SessionID: r.init}) {
				return
			}
		}
		emit(ctx, out, claude.Event{Type: claude.Result, Result: &claude.RunResult{
			Text:      "done",
			SessionID: r.init,
			IsError:   r.isError,
		}})
	}()
	return out, nil
}

// runErrorRunner emits a SystemInit (capturing the session id) then a RunError —
// a process/transport crash, distinct from an is_error Result.
type runErrorRunner struct{ init string }

func (r *runErrorRunner) Run(ctx context.Context, _ string, _ claude.Options) (<-chan claude.Event, error) {
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		if !emit(ctx, out, claude.Event{Type: claude.SystemInit, SessionID: r.init}) {
			return
		}
		emit(ctx, out, claude.Event{Type: claude.RunError, Err: errors.New("transport crash")})
	}()
	return out, nil
}

// TestSessionClearedOnErrorResultWhileResuming is the stale-session self-heal: when a run
// that USED a resume session id terminates with an is_error Result, the stored
// session is cleared so the NEXT message starts fresh (no --resume) and recovers
// from a stale/poisoned session id.
func TestSessionClearedOnErrorResultWhileResuming(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	// Pre-seed a stored session id so this run resumes (opts.SessionID non-empty).
	if err := sess.Set(testChatID, "stale-id"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	fr := &errorResultRunner{init: "stale-id", isError: true}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	// The poisoned session must be cleared so the next message self-heals.
	waitUntil(t, func() bool {
		_, ok := sess.get()
		return !ok
	})
	if dels := sess.deletes(); len(dels) != 1 || dels[0] != testChatID {
		t.Fatalf("Delete calls = %v, want [%q]", dels, testChatID)
	}
}

// TestSessionNotClearedOnCleanResultWhileResuming asserts a CLEAN Result while
// resuming leaves the stored session intact (normal continuity).
func TestSessionNotClearedOnCleanResultWhileResuming(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	if err := sess.Set(testChatID, "good-id"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	fr := &errorResultRunner{init: "good-id", isError: false}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	// The terminal answer lands.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return strings.Contains(text, "done")
	})
	// The session is preserved (and never deleted).
	if id, ok := sess.get(); !ok || id != "good-id" {
		t.Fatalf("session Get = (%q, %v), want (good-id, true)", id, ok)
	}
	if dels := sess.deletes(); len(dels) != 0 {
		t.Fatalf("Delete calls = %v, want none on a clean result", dels)
	}
}

// TestSessionNotClearedOnErrorResultFreshRun asserts an is_error Result on a FRESH
// run (no prior session id, so no resume) is a no-op: nothing to clear, no panic.
func TestSessionNotClearedOnErrorResultFreshRun(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	// No pre-seeded session: the run starts fresh (opts.SessionID empty). The
	// runner still emits a captured init id, which the run persists.
	fr := &errorResultRunner{init: "new-id", isError: true}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	// The terminal answer lands (an is_error Result renders with the warning prefix).
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return strings.Contains(text, "done")
	})
	// No Delete happened (the run was not resuming), and the freshly-captured id is
	// kept so a genuinely new session continues.
	if dels := sess.deletes(); len(dels) != 0 {
		t.Fatalf("Delete calls = %v, want none on a fresh run", dels)
	}
	if id, ok := sess.get(); !ok || id != "new-id" {
		t.Fatalf("session Get = (%q, %v), want (new-id, true)", id, ok)
	}
}

// TestSessionNotClearedOnRunErrorWhileResuming asserts a RunError (process/
// transport crash) while resuming does NOT clear the session — it is likely
// transient, so the context is kept for a retry.
func TestSessionNotClearedOnRunErrorWhileResuming(t *testing.T) {
	fc := newFakeChat()
	sess := newFakeSessions()
	if err := sess.Set(testChatID, "keep-id"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	fr := &runErrorRunner{init: "keep-id"}
	svc, d := newTestServiceWithSessions(t, fr, fc, sess, 0)
	defer d.Close()

	svc.Handle(context.Background(), testChatID, 100, "1", "hello")

	// The terminal error notice lands.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return strings.Contains(text, "transport crash")
	})
	// The session is preserved for a retry.
	if id, ok := sess.get(); !ok || id != "keep-id" {
		t.Fatalf("session Get = (%q, %v), want (keep-id, true)", id, ok)
	}
	if dels := sess.deletes(); len(dels) != 0 {
		t.Fatalf("Delete calls = %v, want none on a RunError", dels)
	}
}

// fixedOutbox resolves every chat to one fixed directory, for the finish-path
// sweep tests.
type fixedOutbox struct{ dir string }

func (f fixedOutbox) OutboxDir(ChatID) (string, error) { return f.dir, nil }

// newTestServiceWithOutbox builds a Service wired to a Sweeper over a fixed
// outbox dir (and an optional per-run timeout) for the AC2 finish-path tests.
func newTestServiceWithOutbox(
	t *testing.T, r claude.Runner, c Transport, dir string, timeout time.Duration,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  &fakeWorkspace{},
		Outbox:     NewSweeper(fixedOutbox{dir: dir}, 0, 0, slog.New(slog.DiscardHandler)),
		Timeout:    timeout,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// TestOutboxSweptOnNormalResult asserts that, on a normal Result, an outbox file
// is delivered as a document AND the text answer is still delivered (AC2).
func TestOutboxSweptOnNormalResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	fc := newFakeChat()
	fr := &fakeRunner{events: []claude.Event{
		{Type: claude.Result, Result: &claude.RunResult{Text: finalAnswer}},
	}}
	svc, d := newTestServiceWithOutbox(t, fr, fc, dir, 0)
	defer d.Close()

	svc.Handle(context.Background(), "100", 100, "1", "go")

	// The text answer reaches the progress message.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
		return text == finalAnswer
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
	if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("data"), 0o600); err != nil {
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

	svc.Handle(context.Background(), "100", 100, "1", "go")
	waitUntil(t, func() bool { return svc.Stop("1") })

	// The terminal "Stopped" notice is delivered.
	waitUntil(t, func() bool {
		text, _ := fc.snapshot()
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
	waitUntilFor(t, 2*time.Second, cond)
}

// waitUntilFor polls cond up to an explicit deadline, failing the test on timeout.
// It lets a test that depends on a real background goroutine (e.g. the progress
// ticker) tolerate scheduler starvation under -race with all packages in parallel —
// a starved tick only makes the poll take longer, never fail spuriously.
func waitUntilFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
