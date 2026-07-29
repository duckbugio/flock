package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/autobudget"
	"github.com/duckbugio/flock/core/followup"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/verify"
)

// AutoUser is the sentinel user id carried by AUTONOMOUS runs — runs no human
// triggered and no stored creator owns: CI-watch fix-ups, post-run verification
// fix-ups, and goal-evaluator follow-ups. No real platform user id is negative,
// so the cost recorder can attribute these runs to the per-chat autonomy budget
// instead of a user. The pre-existing 0 sentinel (poller relays, startup
// resumes) and scheduled fires (attributed to their creator) keep their
// meanings.
const AutoUser int64 = -1

// notifyTimeout bounds one best-effort post-run notice delivery.
const notifyTimeout = 30 * time.Second

// budgetDayFormat renders the UTC day used to de-duplicate the "budget
// reached" notice (one per chat per day).
const budgetDayFormat = "2006-01-02"

// PostRunConfig bundles the autonomous post-run loop dependencies. Every field
// is optional: the zero value disables the whole feature set, keeping existing
// deployments byte-identical.
type PostRunConfig struct {
	// Verifier re-runs changed repos' own check gates after a clean run (the
	// deterministic "trust but verify" pass). Nil disables verification.
	Verifier *verify.Verifier
	// VerifyMaxFixes caps CONSECUTIVE auto-fix injections after failed
	// verification, per chat; the counter resets on a passing round or a real
	// user message. Non-positive disables the fix loop (failures only notify).
	VerifyMaxFixes int
	// Goals persists each chat's /goal objective. Nil disables the evaluator.
	Goals goal.Store
	// GoalMaxAttempts is the default evaluation-round cap for a newly armed goal.
	GoalMaxAttempts int
	// EvalModel overrides the evaluator's --model; empty inherits the run model.
	EvalModel string
	// EvalMaxTurns bounds the evaluator session; non-positive inherits the run cap.
	EvalMaxTurns int
	// EvalTimeout bounds one evaluator session; non-positive means no deadline.
	EvalTimeout time.Duration
	// Budget accumulates autonomous runs' cost per chat per UTC day; nil disables
	// the autonomy budget (autonomous runs are then unmetered).
	Budget *autobudget.Store
	// BudgetCapUSD is the per-chat per-day autonomy ceiling; non-positive
	// disables the gate.
	BudgetCapUSD float64
	// Followups persists the one-shot "come back later" items the workspace
	// followup/ convention schedules (see core/followup). Nil disables the sweep
	// and the promise nudge.
	Followups *followup.FileStore
	// PromiseNudge fires one corrective run when a final answer promises to
	// come back on its own without scheduling anything that would.
	PromiseNudge bool
}

// InjectAuto submits an AUTONOMY-originated prompt (a CI event, a verification
// or goal fix-up) into chatID. It differs from Inject in exactly two ways: the
// run is attributed to AutoUser so its cost lands on the chat's per-day
// autonomy budget, and the injection itself is gated by that budget — once the
// day's budget is spent, autonomous work pauses (with a single daily notice)
// while direct user messages keep working. A fix-up for work already paid for
// should neither be dropped on a busy lane nor lost to a deploy, so it
// enqueues an auto-resume marker and never abandons the job — but it must NOT
// block either: InjectAuto is called from postRun, which executes ON the
// lane's own worker goroutine, and a blocking Submit against that lane's full
// queue would deadlock the chat forever (the worker is the only goroutine that
// drains the queue). Hence TrySubmit first, and on a full lane the blocking
// wait is handed to a detached goroutine — Submit itself is shutdown-safe (it
// drops on the dispatcher's root context), and the marker enqueued above
// resumes the job if the process dies mid-wait. ctx scopes only the budget
// notice — the run itself executes under the Dispatcher's per-chat context.
func (s *Service) InjectAuto(ctx context.Context, chatID ChatID, prompt string) {
	// Before the budget check and before the marker: this path is recurring and
	// unattended (the follow-up sweeper, CI events, verify/goal fix-ups), so an
	// unauthorized provider would otherwise strand one marker per fire.
	if s.blockSubmit(ctx, chatID) {
		return
	}
	if !s.autoAllowed(chatID) {
		s.log.Info("autonomy budget reached; dropping injected run", "chat_id", chatID)
		s.notifyBudgetOnce(ctx, chatID)
		return
	}
	id := s.enqueuePending(chatID, prompt)
	job := func(runCtx context.Context) {
		s.run(runCtx, chatID, AutoUser, prompt, nil, id)
	}
	if s.dispatch.TrySubmit(chatID, job) {
		return
	}
	s.log.Info("lane full; queuing autonomous run in the background", "chat_id", chatID)
	go s.dispatch.Submit(chatID, job)
}

// postRun is the autonomy tail of a CLEANLY successful run: first the
// deterministic gate re-run (verify), then — only on a clean tree — the goal
// evaluator. Each stage may inject a follow-up run; the injected run repeats
// this tail on ITS clean terminal, which is what closes the loop. prompt is
// the prompt that STARTED this run — a verification fix-up carries its loop
// round inside it (see verify.FixPrompt), which is what makes the
// consecutive-round cap survive restarts. ctx is the dispatcher lane context
// (NOT the per-run timeout), so Stop/shutdown still aborts post-run work
// promptly.
func (s *Service) postRun(ctx context.Context, chatID ChatID, workdir, prompt string, snap verify.Snapshot) {
	if ctx.Err() != nil {
		return
	}
	if !s.verifyGates(ctx, chatID, workdir, prompt, snap) {
		return
	}
	s.evaluateGoal(ctx, chatID, workdir)
}

// notify posts a short post-run notice. Delivery must survive the (possibly
// already-cancelled) triggering context, so it runs on a detached, bounded
// derivative. Best-effort: failures are only logged.
func (s *Service) notify(ctx context.Context, chatID ChatID, text string) {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	if _, err := s.chat.Send(sendCtx, chatID, text, "", true); err != nil {
		s.log.Debug("send post-run notice", "chat_id", chatID, "error", err)
	}
}

// verifyGates re-runs the check gates of repos this run changed and reports
// whether the tree is clean (true also when verification is disabled or
// nothing changed). On failure it notifies the chat and — within the
// consecutive-round cap — injects a fix-up run. The round this run represents
// is parsed from ITS OWN prompt (a fix-up prompt carries the marker; anything
// else is round 0), so the cap is durable: a deploy mid-loop resumes the
// fix-up run from its persisted prompt at the same round.
func (s *Service) verifyGates(ctx context.Context, chatID ChatID, workdir, prompt string, snap verify.Snapshot) bool {
	if s.postrun.Verifier == nil {
		return true
	}
	// Re-gate last round's failures even when this run changed nothing —
	// otherwise a no-op "fix" run would silently exit the loop with a red gate.
	s.mu.Lock()
	retry := s.verifyRetry[chatID]
	s.mu.Unlock()
	results := s.postrun.Verifier.Verify(ctx, workdir, snap, retry)
	failed := verify.Failed(results)
	if len(failed) == 0 {
		if len(results) > 0 {
			// A verified-clean round closes any ongoing fix loop.
			s.mu.Lock()
			delete(s.verifyRetry, chatID)
			s.mu.Unlock()
		}
		return true
	}

	var cmds []string
	repos := make([]string, 0, len(failed))
	for _, f := range failed {
		cmds = append(cmds, fmt.Sprintf("%s (`%s`)", f.Repo, f.Command))
		repos = append(repos, f.Repo)
	}
	summary := strings.Join(cmds, ", ")

	round, _ := verify.ParseFixRound(prompt) // 0 for a non-fix-up run
	s.mu.Lock()
	s.verifyRetry[chatID] = repos
	s.mu.Unlock()
	if round >= s.postrun.VerifyMaxFixes {
		s.notify(ctx, chatID, fmt.Sprintf(
			"⚠️ Post-run verification still red after %d auto-fix round(s): %s. Stopping the auto-fix loop — please review.",
			round, summary))
		return false
	}

	s.notify(ctx, chatID, fmt.Sprintf(
		"⚠️ Post-run verification failed: %s. Sending the team back to fix it (round %d/%d).",
		summary, round+1, s.postrun.VerifyMaxFixes))
	s.InjectAuto(ctx, chatID, verify.FixPrompt(failed, round+1, s.postrun.VerifyMaxFixes))
	return false
}

// evaluateGoal runs the independent goal evaluator when the chat has an armed
// /goal, then either celebrates, loops a fix-up run, or stops at the attempt
// cap. Inconclusive rounds (no parsable verdict) never loop and never consume
// an attempt.
func (s *Service) evaluateGoal(ctx context.Context, chatID ChatID, workdir string) {
	if s.postrun.Goals == nil {
		return
	}
	g, ok := s.postrun.Goals.Get(chatID)
	if !ok {
		return
	}
	if !s.autoAllowed(chatID) {
		s.notifyBudgetOnce(ctx, chatID)
		return
	}
	verdict, ok := s.runEvaluator(ctx, chatID, workdir, g.Criterion)
	if !ok {
		s.notify(ctx, chatID, "⚖️ Goal evaluation was inconclusive this round; it will re-run after the next completed run.")
		return
	}
	g, ok, err := s.postrun.Goals.Bump(chatID)
	if err != nil {
		s.log.Error("bump goal attempts", "chat_id", chatID, "error", err)
	}
	if !ok {
		return // goal disarmed concurrently (user ran /goal off)
	}
	switch {
	case verdict.Achieved:
		s.notify(ctx, chatID, fmt.Sprintf(
			"🎯 Goal achieved — confirmed by an independent evaluation (round %d/%d): %s",
			g.Attempts, g.MaxAttempts, verdict.Summary))
		s.deleteGoal(chatID)
	case g.Attempts >= g.MaxAttempts:
		s.notify(ctx, chatID, fmt.Sprintf(
			"⚠️ Goal NOT achieved after %d evaluation round(s) — stopping the loop. Last verdict: %s\nRe-arm with /goal when ready.",
			g.Attempts, verdict.Summary))
		s.deleteGoal(chatID)
	default:
		s.notify(ctx, chatID, fmt.Sprintf(
			"⚖️ Evaluation round %d/%d: goal not met yet — sending the team back to work.",
			g.Attempts, g.MaxAttempts))
		s.InjectAuto(ctx, chatID, goal.FixupPrompt(g.Criterion, verdict))
	}
}

// deleteGoal disarms the chat's goal, logging (never surfacing) a store error.
func (s *Service) deleteGoal(chatID ChatID) {
	if err := s.postrun.Goals.Delete(chatID); err != nil {
		s.log.Error("delete goal", "chat_id", chatID, "error", err)
	}
}

// runEvaluator spawns the evaluator — a FRESH provider session in the same
// workspace (deliberately no session resume: an independent context cannot
// inherit the working session's blind spots) — and parses its verdict. Its cost
// is recorded against the chat's autonomy budget. ok=false means the round was
// inconclusive (spawn/stream error, error result, or unparsable verdict).
func (s *Service) runEvaluator(ctx context.Context, chatID ChatID, workdir, criterion string) (goal.Verdict, bool) {
	opts := s.opts
	opts.SessionID = "" // fresh, independent session
	opts.Workdir = workdir
	opts.Images = nil
	opts.MCPConfig = ""
	opts.Effort = "" // judging needs no ultracode orchestration
	if s.postrun.EvalModel != "" {
		opts.Model = s.postrun.EvalModel
	}
	if s.postrun.EvalMaxTurns > 0 {
		opts.MaxTurns = s.postrun.EvalMaxTurns
	}
	if s.postrun.EvalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.postrun.EvalTimeout)
		defer cancel()
	}

	events, err := s.runner.Run(ctx, goal.EvalPrompt(criterion), opts)
	if err != nil {
		s.log.Error("start goal evaluator", "chat_id", chatID, "error", err)
		return goal.Verdict{}, false
	}
	var res *agent.RunResult
	for ev := range events {
		switch ev.Type {
		case agent.Result:
			res = ev.Result
		case agent.RunError:
			s.log.Error("goal evaluator run", "chat_id", chatID, "error", ev.Err)
		default:
			// Progress events are irrelevant here — the evaluator runs silently.
		}
	}
	if res == nil {
		return goal.Verdict{}, false
	}
	s.recordAutoCost(chatID, res)
	if res.IsError {
		s.log.Warn("goal evaluator returned an error result", "chat_id", chatID, "subtype", res.Subtype)
		return goal.Verdict{}, false
	}
	return goal.ParseVerdict(res.Text)
}

// autoAllowed reports whether autonomous work may still run today for chatID
// under the per-day autonomy budget. Unconfigured budget → always allowed.
func (s *Service) autoAllowed(chatID ChatID) bool {
	return s.postrun.Budget.Allowed(chatID, s.nowFunc(), s.postrun.BudgetCapUSD)
}

// recordAutoCost accumulates a completed autonomous run's cost onto the chat's
// per-day autonomy budget. Nil store / nil result / non-positive cost are
// no-ops; a write failure only weakens the cap, never breaks delivery.
func (s *Service) recordAutoCost(chatID ChatID, res *agent.RunResult) {
	if res == nil {
		return
	}
	if err := s.postrun.Budget.Add(chatID, s.nowFunc(), res.CostUSD); err != nil {
		s.log.Error("record autonomy cost", "chat_id", chatID, "error", err)
	}
}

// notifyBudgetOnce tells the chat its autonomy budget is exhausted — once per
// chat per UTC day, so a busy CI watch or fix loop can't spam the notice.
func (s *Service) notifyBudgetOnce(ctx context.Context, chatID ChatID) {
	day := s.nowFunc().UTC().Format(budgetDayFormat)
	s.mu.Lock()
	already := s.budgetNotified[chatID] == day
	if !already {
		s.budgetNotified[chatID] = day
	}
	s.mu.Unlock()
	if already {
		return
	}
	s.notify(ctx, chatID, fmt.Sprintf(
		"💤 Daily autonomy budget ($%.2f) reached — automated runs (CI reactions, verification and "+
			"goal rounds) are paused until tomorrow. Your direct messages still work.",
		s.postrun.BudgetCapUSD))
}

// ArmGoal arms (or replaces) chatID's goal with the given criterion, using the
// configured default attempt cap. It reports the effective goal for the
// adapter's confirmation reply. ok=false when the goal feature is disabled.
func (s *Service) ArmGoal(chatID ChatID, criterion string) (goal.Goal, bool) {
	if s.postrun.Goals == nil {
		return goal.Goal{}, false
	}
	attempts := s.postrun.GoalMaxAttempts
	if attempts < 1 {
		// Defense in depth behind config.EffectiveGoalMaxAttempts: a non-positive
		// cap would end the loop after its first evaluation round.
		attempts = 1
	}
	g := goal.Goal{
		Criterion:   criterion,
		MaxAttempts: attempts,
		CreatedAt:   s.nowFunc().UnixMilli(),
	}
	if err := s.postrun.Goals.Set(chatID, g); err != nil {
		s.log.Error("arm goal", "chat_id", chatID, "error", err)
		return goal.Goal{}, false
	}
	return g, true
}

// GoalStatus returns chatID's armed goal, ok=false when none (or disabled).
func (s *Service) GoalStatus(chatID ChatID) (goal.Goal, bool) {
	if s.postrun.Goals == nil {
		return goal.Goal{}, false
	}
	return s.postrun.Goals.Get(chatID)
}

// DisarmGoal clears chatID's goal, reporting whether one was armed.
func (s *Service) DisarmGoal(chatID ChatID) bool {
	if s.postrun.Goals == nil {
		return false
	}
	_, ok := s.postrun.Goals.Get(chatID)
	if ok {
		s.deleteGoal(chatID)
	}
	return ok
}
