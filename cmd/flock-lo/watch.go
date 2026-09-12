package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/ciwatch"
	"github.com/duckbugio/flock/core/poller"
	"github.com/duckbugio/flock/internal/config"
)

func ownsReview(base, chatID, repo, branch string) bool {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil || id == 0 {
		return false
	}
	for _, checkout := range ciwatch.Discover(base) {
		if checkout.ChatID == chatID && checkout.Repo == repo && checkout.Branch == branch {
			return true
		}
	}
	return false
}

func startReviewPoller(ctx context.Context, cfg config.Config, svc *chat.Service, logger *slog.Logger) {
	if !cfg.PollerEnabled() {
		return
	}
	out := make(chan poller.PRComment)
	go func() {
		err := poller.Run(ctx, poller.Config{
			BaseURL: cfg.GiteaAPIURL, Token: cfg.GitToken, SelfLogin: strings.ToLower(cfg.GitUser),
			Interval: cfg.GiteaPollDuration(), Logger: logger,
			Accept: func(chatID, repo, branch string) bool { return ownsReview(cfg.ApprovedDirectory, chatID, repo, branch) },
		}, out)
		if err != nil && ctx.Err() == nil {
			logger.Error("LO review poller stopped", "error", err)
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case comment := <-out:
				svc.InjectAuto(ctx, comment.ChatID, fmt.Sprintf(
					"A reviewer left a comment on pull request #%d in %s.\n\nComment by @%s:\n%s\n\n"+
						"Address this review following the Phase 2 review workflow in the existing PR.",
					comment.PRIndex, comment.Repo, comment.Author, comment.Body))
			}
		}
	}()
}

func startCIWatch(ctx context.Context, cfg config.Config, transport *lo.Transport, svc *chat.Service, logger *slog.Logger) {
	if !cfg.CIWatchEnabled() {
		return
	}
	var host ciwatch.Host
	if cfg.CIWatchGitHub() {
		host = ciwatch.NewGitHub(cfg.GitToken, nil)
	} else {
		host = ciwatch.NewGitea(cfg.GiteaAPIURL, cfg.GitToken, nil)
	}
	state, err := ciwatch.OpenState(cfg.CIStateFile())
	if err != nil {
		logger.Error("open LO ci-watch state; watcher disabled", "error", err)
		return
	}
	out := make(chan ciwatch.Event)
	go func() {
		err := ciwatch.Run(ctx, ciwatch.Config{
			BaseDir: cfg.ApprovedDirectory, Host: host,
			Interval: cfg.CIPollDuration(), AutoMerge: cfg.EnableAutoMerge, State: state, Logger: logger,
		}, out)
		if err != nil && ctx.Err() == nil {
			logger.Error("LO CI watch stopped", "error", err)
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-out:
				id, err := strconv.ParseInt(event.ChatID, 10, 64)
				if err != nil || id == 0 {
					logger.Warn("LO CI watch: invalid chat id", "chat_id", event.ChatID)
					continue
				}
				switch event.Kind {
				case ciwatch.CIFailed:
					svc.InjectAuto(ctx, event.ChatID, formatCIFailure(event))
				case ciwatch.CIGreen:
					svc.InjectAuto(ctx, event.ChatID, formatCIGreen(event))
				case ciwatch.PRMerged:
					_, err := transport.Send(ctx, event.ChatID, fmt.Sprintf("CI is green on %s (%s) — auto-merged PR #%d.",
						event.Branch, event.Repo, event.PRIndex), "", false)
					if err != nil {
						logger.Warn("announce LO PR merge", "chat_id", event.ChatID, "error", err)
					}
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
