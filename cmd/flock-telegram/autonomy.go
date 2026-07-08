// Autonomy-loop glue for the Telegram adapter: the /goal command, the CI-watch
// wiring, and the prompt formatters for injected CI events. The mechanics live
// in core/{goal,ciwatch,verify,autobudget} and internal/autonomy; this file
// only parses commands, replies, and routes events into chat.Service.InjectAuto.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/ciwatch"
	"github.com/duckbugio/flock/internal/config"
)

// goalUsage is the /goal help reply (engineering artifact, plain English).
const goalUsage = "Usage:\n" +
	"/goal <done criterion> — arm a goal; after every completed run an independent evaluator " +
	"re-checks the workspace against it and sends the team back until it holds\n" +
	"/goal — show the armed goal\n" +
	"/goal off — disarm"

// goalHandler serves /goal from an allowed user: arm, show, or disarm the
// calling chat's goal.
func goalHandler(cfg config.Config, svc *chat.Service) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		args := commandArgs(update.Message.Text)
		switch strings.ToLower(args) {
		case "":
			if g, armed := svc.GoalStatus(chatIDStr(chatID)); armed {
				sendCommandReply(ctx, b, chatID, fmt.Sprintf(
					"Armed goal (evaluation round %d/%d):\n%s\n\n%s", g.Attempts, g.MaxAttempts, g.Criterion, goalUsage))
				return
			}
			sendCommandReply(ctx, b, chatID, "No goal armed.\n\n"+goalUsage)
		case "off", "clear", "stop":
			if svc.DisarmGoal(chatIDStr(chatID)) {
				sendCommandReply(ctx, b, chatID, "Goal disarmed.")
				return
			}
			sendCommandReply(ctx, b, chatID, "No goal was armed.")
		default:
			g, armed := svc.ArmGoal(chatIDStr(chatID), args)
			if !armed {
				sendCommandReply(ctx, b, chatID, "The goal evaluator is disabled on this deployment.")
				return
			}
			sendCommandReply(ctx, b, chatID, fmt.Sprintf(
				"🎯 Goal armed (up to %d evaluation rounds). After every completed run an independent "+
					"evaluator will judge:\n%s", g.MaxAttempts, g.Criterion))
		}
	}
}

// wireCIWatch starts the CI polling loop and routes its events: a red build is
// injected into the owning chat as a fix-up run; an auto-merged PR is announced
// with a plain notice (no agent run needed).
func wireCIWatch(ctx context.Context, cfg config.Config, b *bot.Bot, svc *chat.Service, logger *slog.Logger) {
	var host ciwatch.Host
	if cfg.CIWatchGitHub() {
		host = ciwatch.NewGitHub(cfg.GitToken, nil)
	} else {
		host = ciwatch.NewGitea(cfg.GiteaAPIURL, cfg.GitToken, nil)
	}
	state, err := ciwatch.OpenState(cfg.CIStateFile())
	if err != nil {
		logger.Error("open ci-watch state; ci watch disabled", "path", cfg.CIStateFile(), "error", err)
		return
	}
	out := make(chan ciwatch.Event)
	go func() {
		if err := ciwatch.Run(ctx, ciwatch.Config{
			BaseDir:   cfg.ApprovedDirectory,
			Host:      host,
			Interval:  cfg.CIPollDuration(),
			AutoMerge: cfg.EnableAutoMerge,
			State:     state,
			Logger:    logger,
		}, out); err != nil && ctx.Err() == nil {
			logger.Error("ci watch stopped", "error", err)
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-out:
				id, err := strconv.ParseInt(ev.ChatID, 10, 64)
				if err != nil {
					logger.Warn("ci watch: bad chat id in branch", "chat_id", ev.ChatID)
					continue
				}
				switch ev.Kind {
				case ciwatch.CIFailed:
					svc.InjectAuto(ctx, ev.ChatID, formatCIFailure(ev))
				case ciwatch.CIGreen:
					svc.InjectAuto(ctx, ev.ChatID, formatCIGreen(ev))
				case ciwatch.PRMerged:
					sendNotice(ctx, b, id, fmt.Sprintf(
						"✅ CI is green on %s (%s) — auto-merged PR #%d.", ev.Branch, ev.Repo, ev.PRIndex))
				}
			}
		}
	}()
}

// shortSHALen is how many SHA characters the CI fix-up prompt shows.
const shortSHALen = 10

// formatCIFailure builds the injected prompt for a red build. Neutral English,
// no token, mirrors formatPRComment's tone.
func formatCIFailure(ev ciwatch.Event) string {
	sha := ev.SHA
	if len(sha) > shortSHALen {
		sha = sha[:shortSHALen]
	}
	detail := ev.Detail
	if detail == "" {
		detail = "no failing-check details were reported"
	}
	return fmt.Sprintf(
		"CI is RED on branch %s in %s (commit %s) — %s.\n\n"+
			"Read the failing check logs via the git host, reproduce the failure locally with the repo's "+
			"own runner, fix it (never weaken or skip tests to get green), and push the fix to the SAME branch.",
		ev.Branch, ev.Repo, sha, detail)
}

// formatCIGreen builds the injected prompt for a green build. Its whole job is
// to WAKE the session: an agent that told its user "I'll report when CI
// finishes" has no other wake-up signal for success (the poller only relays
// new PR comments), so a green build would otherwise end the conversation
// until the user pings.
func formatCIGreen(ev ciwatch.Event) string {
	sha := ev.SHA
	if len(sha) > shortSHALen {
		sha = sha[:shortSHALen]
	}
	return fmt.Sprintf(
		"CI completed SUCCESSFULLY on branch %s in %s (commit %s).\n\n"+
			"If you told the user you would report the CI result, do that now, in their language. "+
			"Otherwise confirm the green build briefly and continue any follow-up you deferred on it "+
			"(e.g. asking the human to merge). Do not re-run the checks.",
		ev.Branch, ev.Repo, sha)
}
