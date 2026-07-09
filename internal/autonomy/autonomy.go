// Package autonomy assembles the post-run autonomy-loop configuration
// (deterministic gate verification, the /goal evaluator, the per-chat autonomy
// budget) shared by every adapter binary — the Telegram and VK mains wire the
// SAME loops, so the builder lives here once instead of drifting per cmd.
package autonomy

import (
	"context"
	"log/slog"

	"github.com/duckbugio/flock/core/autobudget"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/followup"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/verify"
	"github.com/duckbugio/flock/internal/config"
)

// Build opens the autonomy-loop stores and assembles the post-run config.
// Every store-open failure is non-fatal (log + run with that single feature
// disabled), matching the rest of the adapters' best-effort startup wiring.
func Build(cfg config.Config, logger *slog.Logger) chat.PostRunConfig {
	goals, err := goal.Open(cfg.GoalStoreFile())
	if err != nil {
		logger.Error("open goal store; /goal disabled", "path", cfg.GoalStoreFile(), "error", err)
		goals = nil
	}
	budget, err := autobudget.Open(cfg.AutoBudgetStoreFile())
	if err != nil {
		logger.Error("open autonomy budget store; auto budget disabled",
			"path", cfg.AutoBudgetStoreFile(), "error", err)
		budget = nil
	}
	followups, err := followup.Open(cfg.FollowupStoreFile())
	if err != nil {
		logger.Error("open follow-up store; follow-ups disabled", "path", cfg.FollowupStoreFile(), "error", err)
		followups = nil
	}
	postRun := chat.PostRunConfig{
		VerifyMaxFixes:  cfg.PostVerifyMaxFixes,
		GoalMaxAttempts: cfg.EffectiveGoalMaxAttempts(),
		EvalModel:       cfg.EvaluatorModel,
		EvalMaxTurns:    cfg.GoalEvalMaxTurns,
		EvalTimeout:     cfg.GoalEvalTimeout(),
		Budget:          budget,
		BudgetCapUSD:    cfg.AutoTaskMaxCostPerDay,
		Followups:       followups,
		PromiseNudge:    cfg.EnablePromiseNudge,
	}
	if cfg.EnablePostVerify {
		postRun.Verifier = &verify.Verifier{Timeout: cfg.PostVerifyTimeout(), Log: logger}
	}
	if goals != nil {
		postRun.Goals = goals
	}
	logger.Info("autonomy loops configured",
		"post_verify", cfg.EnablePostVerify,
		"goal_evaluator", goals != nil,
		"ci_watch", cfg.CIWatchEnabled(),
		"auto_merge", cfg.CIWatchEnabled() && cfg.EnableAutoMerge,
		"auto_budget_usd_per_day", cfg.AutoTaskMaxCostPerDay,
		"auto_approve_scope", cfg.AutoApproveScopeLevel(),
		"followups", followups != nil,
		"promise_nudge", cfg.EnablePromiseNudge && followups != nil,
	)
	return postRun
}

// StartFollowups runs the one-shot follow-up firing loop for the store wired
// into pr, injecting each due item into its chat through the autonomy-budgeted
// path. A nil store (open failed / feature off) is a no-op.
func StartFollowups(ctx context.Context, svc *chat.Service, pr chat.PostRunConfig, logger *slog.Logger) {
	if pr.Followups == nil {
		return
	}
	go func() {
		if err := followup.Run(ctx, pr.Followups, nil, func(it followup.Item) {
			svc.InjectAuto(ctx, it.ChatID, chat.FollowupPrompt(it))
		}); err != nil && ctx.Err() == nil {
			logger.Error("follow-up loop stopped", "error", err)
		}
	}()
}
