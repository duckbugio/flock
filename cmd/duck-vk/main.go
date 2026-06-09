// Command duck-vk is the Go VKontakte (VK) community-bot adapter for flock. Each
// allowed VK user's DM/mention is dispatched to that chat's isolated workspace
// and run through the core/claude Runner; the event stream renders to one live
// "Working… (Ns)" progress message with a Stop button (edited in place, as VK has
// no ephemeral draft), replaced by the final answer on completion. Runs are
// parallel across chats (capped) and serial within a chat (core/dispatch); each
// chat gets its own /workspace/chat_<peer_id> with a rendered CLAUDE.md + agents
// (core/workspace). It wires the SAME shared core/chat.Service the Telegram
// binary uses — only the adapter construction and the Long Poll receive loop
// differ.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/duckbugio/flock/adapters/vk"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/ghstar"
	"github.com/duckbugio/flock/core/gitsetup"
	"github.com/duckbugio/flock/core/nudge"
	"github.com/duckbugio/flock/core/poller"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/core/session"
	"github.com/duckbugio/flock/core/voice"
	"github.com/duckbugio/flock/core/workspace"
	"github.com/duckbugio/flock/internal/config"
)

// shutdownDrainTimeout bounds how long Shutdown waits for in-flight runs to
// deliver their final message during graceful drain.
const shutdownDrainTimeout = 5 * time.Second

// voiceClientTimeout is the shared budget for the voice file download plus the
// transcription provider call.
const voiceClientTimeout = 60 * time.Second

// uploadClientTimeout is the budget for a (potentially large) inbound file
// download.
const uploadClientTimeout = 120 * time.Second

func main() {
	os.Exit(run())
}

// run holds the adapter's startup and serve logic, returning a process exit code.
// Splitting it out of main lets deferred cleanups (signal context, dispatcher
// drain) run before the process exits.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	if err := cfg.ValidateVK(); err != nil {
		slog.Error("invalid vk config", "error", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Best-effort git startup wiring (identity, default branch, inline credential
	// helper). Non-fatal: a git setup failure must not crash-loop the bot.
	if err := gitsetup.Apply(ctx, gitsetup.Config{
		Host:        cfg.GitHost,
		Scheme:      cfg.GitScheme,
		AuthorName:  cfg.GitAuthorName,
		AuthorEmail: cfg.GitAuthorEmail,
		HasToken:    cfg.GitToken != "",
	}); err != nil {
		logger.Warn("git setup", "error", err)
	}

	runner := claude.New(cfg.ClaudeBin)
	opts := claude.Options{
		Model:    cfg.ClaudeModel,
		MaxTurns: cfg.ClaudeMaxTurns,
		Env:      claudeEnv(cfg),
	}

	ws := &workspace.Renderer{
		BaseDir:        cfg.ApprovedDirectory,
		TemplatePath:   cfg.TeamTemplatePath,
		AgentsDir:      cfg.TeamAgentsDir,
		PrePRCycles:    cfg.PrePRCycles,
		PrReviewCycles: cfg.PrReviewCycles,
		EnablePRReview: cfg.EnablePRReview,
		GitHost:        cfg.GitHost,
	}
	sessions, err := session.Open(cfg.SessionStoreFile())
	if err != nil {
		logger.Error("open session store", "path", cfg.SessionStoreFile(), "error", err)
		return 1
	}

	limiter := ratelimit.New(cfg.RateLimitRequests, cfg.RateLimitWindow())
	costs, err := cost.Open(cfg.CostStoreFile())
	if err != nil {
		logger.Error("open cost store; cost cap disabled", "path", cfg.CostStoreFile(), "error", err)
		costs = nil
	}
	logger.Info("guardrails configured",
		"rate_limit_enabled", cfg.RateLimitEnabled(),
		"rate_limit_requests", cfg.RateLimitRequests,
		"rate_limit_window", cfg.RateLimitWindow(),
		"cost_cap_enabled", cfg.CostCapEnabled() && costs != nil,
		"cost_cap_usd", cfg.ClaudeMaxCostPerUser,
	)

	disp := dispatch.New(cfg.MaxConcurrentChatRuns)
	defer func() {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer cancelDrain()
		if err := disp.Shutdown(drainCtx); err != nil {
			logger.Warn("dispatcher drain timed out", "error", err)
		}
	}()

	// The VK API client (injectable base/client; defaults to the real endpoint).
	api := vk.NewClient(cfg.VKBotToken)
	// Seed the random_id generator from a monotonic source so two process runs don't
	// reuse ids (VK dedupes a repeated random_id within ~1h). Not time-derived in
	// tests (those inject a fixed seed); here a process-start nanosecond seed is fine.
	transport := vk.NewBotChat(api, cfg.VKGroupID, time.Now().UnixNano())

	// Voice transcription (optional, OFF by default). A provider misconfiguration is
	// non-fatal: log and run with voice disabled.
	var vt *vk.VoiceTranscriber
	if cfg.EnableVoiceMessages {
		voiceClient := &http.Client{Timeout: voiceClientTimeout}
		tr, terr := voice.New(voice.Config{
			Provider:      cfg.VoiceProvider,
			MistralAPIKey: cfg.MistralAPIKey,
			OpenAIAPIKey:  cfg.OpenAIAPIKey,
			MistralModel:  cfg.VoiceMistralModel,
			OpenAIModel:   cfg.VoiceOpenAIModel,
			LocalCommand:  cfg.VoiceLocalCommand,
			HTTPClient:    voiceClient,
			Logger:        logger,
		})
		if terr != nil {
			logger.Warn("voice disabled", "error", terr)
		} else {
			vt = vk.NewVoiceTranscriber(voiceClient, tr, 0, logger)
			logger.Info("voice transcription enabled", "provider", cfg.VoiceProvider)
		}
	}

	// Inbound document/photo uploads. Always enabled: files are saved to the per-chat
	// uploads dir (outside every git tree), the URL redacted from any error.
	uploadClient := &http.Client{Timeout: uploadClientTimeout}
	up := vk.NewUploader(uploadClient, ws, cfg.MaxUploadBytes, logger)

	// Outbound file delivery ("outbox"): after each run, sweep the per-chat outbox
	// dir and send each produced file as a VK document.
	outbox := chat.NewSweeper(ws, cfg.MaxOutboxBytes, cfg.MaxOutboxFiles, logger)

	// Post-task GitHub star nudge (GitHub-only; the gate is the off switch).
	starNudge := buildStarNudge(cfg, logger)

	svc := chat.New(chat.Config{
		Runner:     runner,
		Transport:  transport,
		Dispatcher: disp,
		Workspace:  ws,
		Sessions:   sessions,
		Costs:      costs,
		Outbox:     outbox,
		StarNudge:  starNudge,
		Opts:       opts,
		Timeout:    cfg.ClaudeTimeout(),
		RetryAfter: vk.RetryAfter,
		Logger:     logger,
	})

	guards := chat.GuardConfig{CostCapUSD: cfg.ClaudeMaxCostPerUser}
	receiver := vk.NewReceiver(vk.ReceiverConfig{
		Service:        svc,
		GroupID:        cfg.VKGroupID,
		RequireMention: cfg.RequireGroupMention,
		IsAllowed:      cfg.IsVKAllowed,
		Guards: func(userID int64) (bool, string) {
			return chat.CheckGuards(limiter, costs, guards, userID)
		},
		Voice:    voiceTranscriber(vt),
		Uploads:  up,
		Notices:  vk.NewNoticeSender(api, time.Now().UnixNano(), logger),
		EventAck: api.SendMessageEventAnswer,
		Logger:   logger,
	})

	// PR poller (same as Telegram): relay PR review comments on a duck/<chatid>/… PR
	// into the owning chat. Transport-neutral (keyed on the string chat id).
	if cfg.PollerEnabled() {
		out := make(chan poller.PRComment)
		go func() {
			if err := poller.Run(ctx, poller.Config{
				BaseURL:   cfg.GiteaAPIURL,
				Token:     cfg.GitToken,
				SelfLogin: strings.ToLower(cfg.GitUser),
				Interval:  cfg.GiteaPollDuration(),
				Logger:    logger,
			}, out); err != nil && ctx.Err() == nil {
				logger.Error("poller stopped", "error", err)
			}
		}()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case c := <-out:
					if _, err := strconv.ParseInt(c.ChatID, 10, 64); err != nil {
						logger.Warn("poller: bad chat id in branch", "chat_id", c.ChatID)
						continue
					}
					svc.Inject(c.ChatID, formatPRComment(c))
				}
			}
		}()
	}

	logger.Info("starting vk adapter", "group_id", cfg.VKGroupID)
	err = receiver.Run(ctx, api)
	if err != nil && ctx.Err() == nil {
		logger.Error("vk adapter stopped", "error", err)
		return 1
	}
	logger.Info("vk adapter stopped")
	return 0
}

// voiceTranscriber adapts a possibly-nil *vk.VoiceTranscriber to the receiver's
// transcriber interface: a nil concrete pointer must be passed as a nil interface
// so the receiver's nil-check disables voice (passing a typed-nil pointer would
// make the interface non-nil and panic on call).
func voiceTranscriber(vt *vk.VoiceTranscriber) vkTranscriber {
	if vt == nil {
		return nil
	}
	return vt
}

// vkTranscriber mirrors the receiver's transcriber seam so the cmd can hand a nil
// interface when voice is disabled.
type vkTranscriber interface {
	Transcribe(ctx context.Context, url string) (string, error)
}

// buildStarNudge assembles the post-task star-nudge config (GitHub-only; disabled
// otherwise). Mirrors cmd/flock-telegram.
func buildStarNudge(cfg config.Config, logger *slog.Logger) chat.StarNudgeConfig {
	if !cfg.StarNudgeEnabled() {
		return chat.StarNudgeConfig{}
	}
	owner, repo, ok := cfg.StarNudgeRepoParts()
	if !ok {
		return chat.StarNudgeConfig{}
	}
	store, err := nudge.Open(cfg.StarNudgeStoreFile())
	if err != nil {
		logger.Error("open star-nudge store; nudge disabled", "path", cfg.StarNudgeStoreFile(), "error", err)
		return chat.StarNudgeConfig{}
	}
	logger.Info("star nudge enabled", "repo", cfg.StarNudgeRepo)
	return chat.StarNudgeConfig{
		Enabled: true,
		Owner:   owner,
		Repo:    repo,
		Client:  ghstar.New(ghstar.Config{Token: cfg.GitToken, Logger: logger}),
		Store:   store,
	}
}

// claudeEnv derives the child process environment for the claude CLI. Mirrors
// cmd/flock-telegram.
func claudeEnv(cfg config.Config) []string {
	env := os.Environ()
	if cfg.ClaudeCodeOAuthToken != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+cfg.ClaudeCodeOAuthToken)
	}
	if cfg.AnthropicAPIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+cfg.AnthropicAPIKey)
	}
	return env
}

// formatPRComment builds a neutral English prompt relaying a polled PR review
// comment into a chat. Mirrors cmd/flock-telegram (an engineering artifact, no
// duck flavor, no token).
func formatPRComment(c poller.PRComment) string {
	return fmt.Sprintf(
		"A reviewer left a comment on pull request #%d in %s (branch duck/%s/…).\n\n"+
			"Comment by @%s:\n%s\n\n"+
			"Please address this PR review comment following the Phase 2 review workflow.",
		c.PRIndex, c.Repo, c.ChatID, c.Author, c.Body,
	)
}
