package schedule_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/schedule"
)

// recordingFire records every (chatID, prompt) the scheduler fires, race-safe so
// it can be read while Run is ticking on another goroutine.
type recordingFire struct {
	mu    sync.Mutex
	calls []fireCall
}

type fireCall struct{ chatID, prompt string }

func (r *recordingFire) fire(chatID, prompt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fireCall{chatID, prompt})
}

func (r *recordingFire) seen() []fireCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fireCall(nil), r.calls...)
}

// newManager builds a Manager over a fresh temp store with a stubbed clock and a
// recording fire callback. The returned now pointer lets a test step time
// deterministically (the Manager reads *now through the closure).
func newManager(t *testing.T) (*schedule.Manager, *schedule.Store, *recordingFire, *time.Time) {
	t.Helper()
	store, err := schedule.Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := &recordingFire{}
	clock := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	nowPtr := &clock
	mgr := schedule.NewManager(store, rec.fire, func() time.Time { return *nowPtr }, nil)
	return mgr, store, rec, nowPtr
}

// newDispatchManager builds a Manager + store for the Dispatch tests, which never
// step the clock or fire jobs (a no-op fire, the real wall clock).
func newDispatchManager(t *testing.T) (*schedule.Manager, *schedule.Store) {
	t.Helper()
	store, err := schedule.Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return schedule.NewManager(store, func(string, string) {}, nil, nil), store
}

// --- Dispatch tests ---

func TestDispatchUsage(t *testing.T) {
	mgr, _ := newDispatchManager(t)
	for _, args := range []string{"", "   ", "bogus", "wat now"} {
		if got := mgr.Dispatch("100", 7, args); !strings.HasPrefix(got, "Usage:") {
			t.Errorf("Dispatch(%q) = %q, want a usage string", args, got)
		}
	}
}

func TestDispatchAddValid(t *testing.T) {
	mgr, store := newDispatchManager(t)
	reply := mgr.Dispatch("100", 42, "add standup 0 9 * * 1 run the morning report")
	if !strings.Contains(reply, "Added job #") {
		t.Fatalf("add reply = %q, want 'Added job #...'", reply)
	}
	jobs := store.List("100")
	if len(jobs) != 1 {
		t.Fatalf("store has %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Name != "standup" || j.Cron != "0 9 * * 1" || j.Prompt != "run the morning report" {
		t.Errorf("persisted job = %+v, want name=standup cron='0 9 * * 1' prompt='run the morning report'", j)
	}
	if j.CreatedBy != 42 || !j.Active {
		t.Errorf("persisted job CreatedBy=%d Active=%v, want 42/true", j.CreatedBy, j.Active)
	}
}

func TestDispatchAddInvalidCron(t *testing.T) {
	mgr, store := newDispatchManager(t)
	reply := mgr.Dispatch("100", 1, "add bad 99 9 * * 1 do it")
	if !strings.Contains(strings.ToLower(reply), "invalid cron") {
		t.Errorf("invalid-cron reply = %q, want an 'Invalid cron' rejection", reply)
	}
	if got := len(store.List("100")); got != 0 {
		t.Errorf("invalid cron persisted %d jobs, want 0", got)
	}
}

func TestDispatchAddEmptyPrompt(t *testing.T) {
	mgr, store := newDispatchManager(t)
	// Name + 5 cron fields but no prompt.
	reply := mgr.Dispatch("100", 1, "add noprompt 0 9 * * 1")
	if !strings.Contains(strings.ToLower(reply), "usage") && !strings.Contains(strings.ToLower(reply), "prompt") {
		t.Errorf("empty-prompt reply = %q, want a prompt/usage rejection", reply)
	}
	if got := len(store.List("100")); got != 0 {
		t.Errorf("empty prompt persisted %d jobs, want 0", got)
	}
}

func TestDispatchListEmptyAndPopulated(t *testing.T) {
	mgr, _ := newDispatchManager(t)
	if got := mgr.Dispatch("100", 1, "list"); !strings.Contains(got, "No scheduled jobs") {
		t.Errorf("empty list = %q, want a 'No scheduled jobs' notice", got)
	}
	mgr.Dispatch("100", 1, "add a 0 9 * * 1 prompt a")
	mgr.Dispatch("100", 1, "add b 0 10 * * 1 prompt b")
	got := mgr.Dispatch("100", 1, "list")
	if !strings.Contains(got, "#1") || !strings.Contains(got, "#2") {
		t.Errorf("populated list = %q, want both #1 and #2", got)
	}
	if !strings.Contains(got, "active") {
		t.Errorf("populated list = %q, want the active state shown", got)
	}
}

func TestDispatchRemovePauseResume(t *testing.T) {
	mgr, store := newDispatchManager(t)
	mgr.Dispatch("100", 1, "add a 0 9 * * 1 prompt a") // job #1

	if got := mgr.Dispatch("100", 1, "pause 1"); !strings.Contains(got, "Paused job #1") {
		t.Errorf("pause = %q, want 'Paused job #1'", got)
	}
	if jobs := store.List("100"); len(jobs) != 1 || jobs[0].Active {
		t.Errorf("after pause job Active=%v, want false", jobs[0].Active)
	}
	if got := mgr.Dispatch("100", 1, "resume 1"); !strings.Contains(got, "Resumed job #1") {
		t.Errorf("resume = %q, want 'Resumed job #1'", got)
	}
	if jobs := store.List("100"); !jobs[0].Active {
		t.Errorf("after resume job Active=false, want true")
	}
	if got := mgr.Dispatch("100", 1, "remove 1"); !strings.Contains(got, "Removed job #1") {
		t.Errorf("remove = %q, want 'Removed job #1'", got)
	}
	if got := store.List("100"); got != nil {
		t.Errorf("after remove List = %+v, want nil", got)
	}
}

func TestDispatchUnknownIDNotFound(t *testing.T) {
	mgr, _ := newDispatchManager(t)
	for _, sub := range []string{"remove 99", "pause 99", "resume 99"} {
		if got := mgr.Dispatch("100", 1, sub); !strings.Contains(strings.ToLower(got), "not found") {
			t.Errorf("Dispatch(%q) = %q, want a 'not found'", sub, got)
		}
	}
}

// TestDispatchForeignChatScoping asserts a chat can never mutate another chat's
// job: an id that belongs to chat 200 is "not found" for chat 100, and the job
// stays untouched.
func TestDispatchForeignChatScoping(t *testing.T) {
	mgr, store := newDispatchManager(t)
	mgr.Dispatch("200", 1, "add a 0 9 * * 1 prompt a") // job #1 belongs to chat 200

	if got := mgr.Dispatch("100", 1, "remove 1"); !strings.Contains(strings.ToLower(got), "not found") {
		t.Errorf("foreign remove = %q, want 'not found'", got)
	}
	if got := mgr.Dispatch("100", 1, "pause 1"); !strings.Contains(strings.ToLower(got), "not found") {
		t.Errorf("foreign pause = %q, want 'not found'", got)
	}
	// Chat 200's job is intact and still active.
	if jobs := store.List("200"); len(jobs) != 1 || !jobs[0].Active {
		t.Errorf("chat 200 job was disturbed: %+v", store.List("200"))
	}
}

// TestDispatchAddPreservesPromptSpacing asserts the prompt keeps its internal
// spacing rather than being re-joined with single spaces.
func TestDispatchAddPreservesPromptSpacing(t *testing.T) {
	mgr, store := newDispatchManager(t)
	mgr.Dispatch("100", 1, "add a 0 9 * * 1 do  the   thing")
	jobs := store.List("100")
	if len(jobs) != 1 || jobs[0].Prompt != "do  the   thing" {
		t.Errorf("prompt = %q, want 'do  the   thing' (internal spacing preserved)", jobs[0].Prompt)
	}
}

// TestDispatchAddRejectsPastCap asserts that once a chat holds MaxJobsPerChat
// jobs, `add` is refused with a clear message and nothing extra is persisted.
func TestDispatchAddRejectsPastCap(t *testing.T) {
	mgr, store := newDispatchManager(t)
	for i := 0; i < schedule.MaxJobsPerChat; i++ {
		if reply := mgr.Dispatch("100", 1, "add j 0 9 * * 1 do it"); !strings.Contains(reply, "Added job #") {
			t.Fatalf("add within cap reply = %q, want 'Added job #...'", reply)
		}
	}
	reply := mgr.Dispatch("100", 1, "add over 0 9 * * 1 do it")
	if !strings.Contains(strings.ToLower(reply), "maximum") {
		t.Errorf("past-cap reply = %q, want a 'maximum' rejection", reply)
	}
	if got := len(store.List("100")); got != schedule.MaxJobsPerChat {
		t.Errorf("chat has %d jobs after the rejected add, want the cap %d", got, schedule.MaxJobsPerChat)
	}
}

// --- Scheduler firing (deterministic, no real time) ---

// TestSchedulerFiresOncePerMatchingMinute drives tickOnce across several
// sub-minute ticks within the same matching minute and asserts the job fires
// exactly once (minute-key de-dup), then again on the next matching minute.
func TestSchedulerFiresOncePerMatchingMinute(t *testing.T) {
	mgr, store, rec, now := newManager(t)
	// A job that matches at minute 0 of every hour.
	if _, err := store.Add(schedule.Job{ChatID: "100", Name: "j", Cron: "0 * * * *", Prompt: "go", Active: true}); err != nil {
		t.Fatal(err)
	}

	*now = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	mgr.TickOnce()
	*now = time.Date(2026, 6, 18, 9, 0, 30, 0, time.UTC) // same minute, sub-minute tick
	mgr.TickOnce()
	if got := rec.seen(); len(got) != 1 {
		t.Fatalf("fired %d times in one matching minute, want exactly 1 (de-dup): %+v", len(got), got)
	}
	if rec.seen()[0] != (fireCall{"100", "go"}) {
		t.Errorf("fired %+v, want {100, go}", rec.seen()[0])
	}

	// Advance to the next matching minute (10:00): it fires again.
	*now = time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	mgr.TickOnce()
	if got := rec.seen(); len(got) != 2 {
		t.Fatalf("fired %d times across two matching minutes, want 2", len(got))
	}
}

// TestSchedulerSkipsNonMatchingMinute asserts a tick during a minute the job does
// not match fires nothing.
func TestSchedulerSkipsNonMatchingMinute(t *testing.T) {
	mgr, store, rec, now := newManager(t)
	if _, err := store.Add(schedule.Job{ChatID: "100", Name: "j", Cron: "0 * * * *", Prompt: "go", Active: true}); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 6, 18, 9, 1, 0, 0, time.UTC) // minute 1, job wants minute 0
	mgr.TickOnce()
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("fired %d times on a non-matching minute, want 0", len(got))
	}
}

// TestSchedulerPausedNeverFires asserts a paused job is never fired even when its
// schedule matches the current minute.
func TestSchedulerPausedNeverFires(t *testing.T) {
	mgr, store, rec, now := newManager(t)
	if _, err := store.Add(schedule.Job{ChatID: "100", Name: "j", Cron: "* * * * *", Prompt: "go", Active: false}); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	mgr.TickOnce()
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("paused job fired %d times, want 0", len(got))
	}
}

// TestSchedulerHotReload asserts a job added between ticks (the LIVE store is read
// each tick) fires on its next matching minute with no restart.
func TestSchedulerHotReload(t *testing.T) {
	mgr, store, rec, now := newManager(t)
	*now = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	mgr.TickOnce() // nothing scheduled yet
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("fired %d before any job added, want 0", len(got))
	}

	// Add a job that matches the NEXT minute.
	if _, err := store.Add(schedule.Job{ChatID: "100", Name: "j", Cron: "1 * * * *", Prompt: "go", Active: true}); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 6, 18, 9, 1, 0, 0, time.UTC)
	mgr.TickOnce()
	if got := rec.seen(); len(got) != 1 {
		t.Fatalf("hot-reloaded job fired %d times, want 1", len(got))
	}
}

// TestSchedulerInvalidCronSkipped asserts a stored job whose cron no longer parses
// is skipped (logged, never fired) rather than panicking the tick.
func TestSchedulerInvalidCronSkipped(t *testing.T) {
	mgr, store, rec, now := newManager(t)
	// Inject an unparseable cron directly (Dispatch would reject it, but a corrupt
	// store or a future format change could carry one).
	if _, err := store.Add(schedule.Job{ChatID: "100", Name: "j", Cron: "bogus", Prompt: "go", Active: true}); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	mgr.TickOnce()
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("invalid-cron job fired %d times, want 0", len(got))
	}
}

// TestRunStopsOnContextCancel asserts Run exits when its context is cancelled
// (the background loop is cancelable for clean shutdown).
func TestRunStopsOnContextCancel(t *testing.T) {
	mgr, _ := newDispatchManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil after cancel, want ctx.Err()")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}
