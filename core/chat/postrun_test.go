//nolint:testpackage // intentionally whitebox to test unexported post-run loop internals
package chat

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/autobudget"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/followup"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/verify"
)

// scriptedRunner returns a fresh scripted event slice per Run call (call 1 =
// script[0], …), recording every prompt. onRun, when non-nil, fires at the
// START of each call — a test hook to mutate the workspace "during" a run.
type scriptedRunner struct {
	mu      sync.Mutex
	script  [][]agent.Event
	prompts []string
	onRun   func(call int)
}

func (r *scriptedRunner) Run(ctx context.Context, prompt string, _ agent.Options) (<-chan agent.Event, error) {
	r.mu.Lock()
	call := len(r.prompts)
	r.prompts = append(r.prompts, prompt)
	var events []agent.Event
	if call < len(r.script) {
		events = r.script[call]
	}
	hook := r.onRun
	r.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	out := make(chan agent.Event)
	go func() {
		defer close(out)
		for _, e := range events {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (r *scriptedRunner) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

// cleanResult builds a successful terminal Result event with the given cost.
func cleanResult(text string, cost float64) []agent.Event {
	return []agent.Event{{Type: agent.Result, Result: &agent.RunResult{Text: text, CostUSD: cost}}}
}

// dirWorkspace resolves every chat to one fixed real directory (unlike
// fakeWorkspace's synthetic path), so verifier tests can plant repos in it.
type dirWorkspace struct{ dir string }

func (w *dirWorkspace) Ensure(ChatID) (string, error) { return w.dir, nil }

// newPostRunService wires a Service with the given runner + post-run config.
func newPostRunService(
	t *testing.T, r agent.Runner, c Transport, ws workspaceEnsurer, pr PostRunConfig,
) (*Service, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(4)
	s := New(Config{
		Runner:     r,
		Transport:  c,
		Dispatcher: d,
		Workspace:  ws,
		PostRun:    pr,
		Logger:     slog.New(slog.DiscardHandler),
	})
	s.tick = 5 * time.Millisecond
	return s, d
}

// openGoals opens a real goal store in a temp dir.
func openGoals(t *testing.T) *goal.FileStore {
	t.Helper()
	s, err := goal.Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestGoalLoopFixupThenAchieved drives the full /goal loop: run → evaluator
// says "not met" → fix-up injected → evaluator says "met" → goal disarmed.
func TestGoalLoopFixupThenAchieved(t *testing.T) {
	goals := openGoals(t)
	if err := goals.Set("100", goal.Goal{Criterion: "the dashboard loads", MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{
		cleanResult("did some work", 0),
		cleanResult(`{"achieved": false, "summary": "endpoint missing", "unmet": ["GET /dash 404s"]}`, 0),
		cleanResult("fixed the endpoint", 0),
		cleanResult(`{"achieved": true, "summary": "dashboard loads fine", "unmet": []}`, 0),
	}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{Goals: goals, GoalMaxAttempts: 3})
	defer d.Close()

	svc.Handle(context.Background(), "100", 42, "1", "build the dashboard")

	waitUntil(t, func() bool { _, armed := goals.Get("100"); return !armed })
	prompts := fr.recorded()
	if len(prompts) != 4 {
		t.Fatalf("want 4 runs (work, eval, fixup, eval), got %d: %q", len(prompts), prompts)
	}
	if !strings.Contains(prompts[1], "the dashboard loads") || !strings.Contains(prompts[1], "acceptance evaluator") {
		t.Fatalf("run 2 must be the evaluator, got: %s", prompts[1])
	}
	if !strings.Contains(prompts[2], "GET /dash 404s") {
		t.Fatalf("run 3 must be the fix-up carrying the unmet point, got: %s", prompts[2])
	}
	joined := strings.Join(allSent(fc), "\n")
	if !strings.Contains(joined, "🎯") {
		t.Fatalf("achievement notice missing; sent:\n%s", joined)
	}
}

// TestGoalLoopStopsAtAttemptCap asserts the loop ends (goal disarmed, no
// fix-up injected) once the evaluation-round cap is reached.
func TestGoalLoopStopsAtAttemptCap(t *testing.T) {
	goals := openGoals(t)
	if err := goals.Set("100", goal.Goal{Criterion: "impossible", MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{
		cleanResult("did some work", 0),
		cleanResult(`{"achieved": false, "summary": "still impossible", "unmet": ["x"]}`, 0),
	}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{Goals: goals, GoalMaxAttempts: 1})
	defer d.Close()

	svc.Handle(context.Background(), "100", 42, "1", "try")

	waitUntil(t, func() bool { _, armed := goals.Get("100"); return !armed })
	// Give a would-be fix-up a moment to (wrongly) appear, then assert none did.
	time.Sleep(50 * time.Millisecond)
	if got := len(fr.recorded()); got != 2 {
		t.Fatalf("want exactly 2 runs (work + eval), got %d: %q", got, fr.recorded())
	}
	if joined := strings.Join(allSent(fc), "\n"); !strings.Contains(joined, "NOT achieved") {
		t.Fatalf("cap notice missing; sent:\n%s", joined)
	}
}

// TestInjectBudgetGate asserts a spent autonomy budget drops injected runs with
// a single daily notice, while leaving them unmetered when no budget is wired.
func TestInjectBudgetGate(t *testing.T) {
	budget, err := autobudget.Open(filepath.Join(t.TempDir(), "auto.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.Add("100", time.Now(), 5); err != nil { // cap below spend
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{cleanResult("hi", 0)}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{Budget: budget, BudgetCapUSD: 2})
	defer d.Close()

	svc.InjectAuto(context.Background(), "100", "ci fix-up")
	svc.InjectAuto(context.Background(), "100", "another ci fix-up")

	waitUntil(t, func() bool { return len(allSent(fc)) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := fr.recorded(); len(got) != 0 {
		t.Fatalf("budget-gated Inject must not run, got %q", got)
	}
	notices := 0
	for _, s := range allSent(fc) {
		if strings.Contains(s, "autonomy budget") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("want exactly one budget notice per day, got %d", notices)
	}
}

// TestAutoRunCostAccruesToBudget asserts an injected run's cost lands on the
// chat's autonomy budget, not on any user's cost total.
func TestAutoRunCostAccruesToBudget(t *testing.T) {
	budget, err := autobudget.Open(filepath.Join(t.TempDir(), "auto.json"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{cleanResult("done", 1.5)}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{Budget: budget, BudgetCapUSD: 10})
	defer d.Close()

	svc.InjectAuto(context.Background(), "100", "ci fix-up")

	waitUntil(t, func() bool { return budget.SpentToday("100", time.Now()) > 1.4 })
	if got := budget.SpentToday("100", time.Now()); got != 1.5 {
		t.Fatalf("SpentToday = %v, want 1.5", got)
	}
}

// TestVerifyLoopInjectsFixupAndStopsAtCap exercises the deterministic gate
// loop end to end with a real repo whose gate always fails: run 1 changes the
// repo → verification fails → fix-up round 1 → (no-op fix, retry list re-gates)
// → fix-up round 2 → cap reached → loop stops with a final notice.
func TestVerifyLoopInjectsFixupAndStopsAtCap(t *testing.T) {
	for _, tool := range []string{"git", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	ws := t.TempDir()
	repoDir := filepath.Join(ws, "app")
	if err := os.MkdirAll(repoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	gitRun := func(args ...string) {
		t.Helper()
		//nolint:gosec,noctx // test helper drives git with fixed args; no ctx needed
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "Makefile"), []byte("check:\n\t@echo red gate; exit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun("init", "-q")
	gitRun("add", ".")
	gitRun("-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")

	fr := &scriptedRunner{
		script: [][]agent.Event{
			cleanResult("changed the repo", 0), // user run
			cleanResult("claimed a fix", 0),    // fix-up 1 (touches nothing)
			cleanResult("claimed again", 0),    // fix-up 2 (touches nothing)
		},
		onRun: func(call int) {
			if call == 0 { // mutate the repo DURING the user run so it registers as changed
				_ = os.WriteFile(filepath.Join(repoDir, "changed.txt"), []byte("x\n"), 0o600)
			}
		},
	}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &dirWorkspace{dir: ws}, PostRunConfig{
		Verifier:       &verify.Verifier{Timeout: time.Minute, Log: slog.New(slog.DiscardHandler)},
		VerifyMaxFixes: 2,
	})
	defer d.Close()

	svc.Handle(context.Background(), "100", 42, "1", "change the repo")

	// The loop ends with the "still red … stopping" notice after 2 fix rounds.
	waitUntil(t, func() bool {
		return strings.Contains(strings.Join(allSent(fc), "\n"), "Stopping the auto-fix loop")
	})
	prompts := fr.recorded()
	if len(prompts) != 3 {
		t.Fatalf("want 3 runs (user + 2 capped fix-ups), got %d: %q", len(prompts), prompts)
	}
	for _, p := range prompts[1:] {
		if !strings.Contains(p, "Post-run verification") || !strings.Contains(p, "make check") {
			t.Fatalf("fix-up prompt must carry the failing gate, got: %s", p)
		}
	}
}

// allSent snapshots every text the fake chat delivered (sends only).
func allSent(f *fakeChat) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// TestInjectAutoNeverBlocksOnFullLane guards the deadlock fix: InjectAuto is
// called from postRun, which executes ON the lane's own worker goroutine, so
// it must return promptly even when the chat's queue is completely full — a
// blocking submit there would deadlock the chat forever (the worker is the
// only goroutine that drains the queue).
func TestInjectAutoNeverBlocksOnFullLane(t *testing.T) {
	fr := &scriptedRunner{}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{})
	defer d.Close()

	// Occupy the lane worker, then fill the queue buffer completely.
	gate := make(chan struct{})
	svc.dispatch.Submit("100", func(context.Context) { <-gate })
	for i := 0; i < 64; i++ {
		if !svc.dispatch.(*dispatch.Dispatcher).TrySubmit("100", func(context.Context) {}) {
			break // buffer full — exactly the state under test
		}
	}

	done := make(chan struct{})
	go func() {
		svc.InjectAuto(context.Background(), "100", "ci fix-up on a saturated lane")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("InjectAuto blocked on a full lane — lane-deadlock regression")
	}
	close(gate)
}

// TestFollowupSweepSchedules asserts a followup/<delay>.md file written during
// a run is scheduled into the durable store, the file is consumed, and the
// chat gets the confirmation notice.
func TestFollowupSweepSchedules(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "followup"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "followup", "15m.md"), []byte("re-check the deploy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := followup.Open(filepath.Join(t.TempDir(), "followups.json"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{cleanResult("done", 0)}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &dirWorkspace{dir: ws}, PostRunConfig{
		Followups: store, PromiseNudge: true,
	})
	defer d.Close()

	svc.Handle(context.Background(), "100", 42, "1", "deploy it")

	waitUntil(t, func() bool {
		_, statErr := os.Stat(filepath.Join(ws, "followup", "15m.md"))
		return store.Count("100") == 1 && os.IsNotExist(statErr)
	})
	if joined := strings.Join(allSent(fc), "\n"); !strings.Contains(joined, "Follow-up scheduled") {
		t.Fatalf("confirmation notice missing; sent:\n%s", joined)
	}
	// A scheduled follow-up suppresses the promise nudge even if the answer
	// promised — exactly one run happened.
	time.Sleep(50 * time.Millisecond)
	if got := len(fr.recorded()); got != 1 {
		t.Fatalf("want exactly 1 run, got %d: %q", got, fr.recorded())
	}
}

// TestPromiseNudgeFiresOnceAndNeverChains asserts a final answer that promises
// an unprompted return triggers exactly ONE corrective run — even when that
// corrective run's own answer promises again.
func TestPromiseNudgeFiresOnceAndNeverChains(t *testing.T) {
	store, err := followup.Open(filepath.Join(t.TempDir(), "followups.json"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{
		cleanResult("Deploy started. I'll report back once it completes.", 0),
		cleanResult("Понял. Доложу, как только процесс завершится.", 0), // the nudge run promises AGAIN
	}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{
		Followups: store, PromiseNudge: true,
	})
	defer d.Close()

	svc.Handle(context.Background(), "100", 42, "1", "deploy it and tell me")

	waitUntil(t, func() bool { return len(fr.recorded()) == 2 })
	time.Sleep(100 * time.Millisecond)
	prompts := fr.recorded()
	if len(prompts) != 2 {
		t.Fatalf("want exactly 2 runs (work + one nudge), got %d: %q", len(prompts), prompts)
	}
	if !strings.Contains(prompts[1], "Self-follow-up check.") {
		t.Fatalf("second run must be the corrective nudge, got: %s", prompts[1])
	}
}

// TestNoNudgeOnPlainAnswer asserts ordinary answers (including ones asking the
// USER to follow up) do not trigger the corrective run.
func TestNoNudgeOnPlainAnswer(t *testing.T) {
	store, err := followup.Open(filepath.Join(t.TempDir(), "followups.json"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &scriptedRunner{script: [][]agent.Event{
		cleanResult("Готово: PR открыт. Напиши мне, если CI упадёт — сам я его не увижу.", 0),
	}}
	fc := newFakeChat()
	svc, d := newPostRunService(t, fr, fc, &fakeWorkspace{}, PostRunConfig{
		Followups: store, PromiseNudge: true,
	})
	defer d.Close()

	svc.Handle(context.Background(), "100", 42, "1", "open the pr")

	waitUntil(t, func() bool { return len(fr.recorded()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := len(fr.recorded()); got != 1 {
		t.Fatalf("plain answer must not nudge, got %d runs: %q", got, fr.recorded())
	}
}
