// Command flock-telegram is the Go Telegram adapter for flock. Each allowed
// user's message is dispatched to that chat's isolated workspace and run through
// the configured AI provider Runner; the event stream renders to one live "Working… (Ns)"
// progress message with a Stop button, replaced by the final answer on
// completion. Runs are parallel across chats (capped) and serial within a chat
// (core/dispatch); each chat gets its own /workspace/chat_<id> with a rendered
// CLAUDE.md + agents (core/workspace).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed IANA tz database so /schedule per-chat timezones work without OS zoneinfo

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/adapters/telegram"
	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/codexauth"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/ghstar"
	"github.com/duckbugio/flock/core/gitsetup"
	"github.com/duckbugio/flock/core/nudge"
	"github.com/duckbugio/flock/core/pending"
	"github.com/duckbugio/flock/core/poller"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/core/schedule"
	"github.com/duckbugio/flock/core/session"
	"github.com/duckbugio/flock/core/voice"
	"github.com/duckbugio/flock/core/workspace"
	"github.com/duckbugio/flock/internal/airunner"
	"github.com/duckbugio/flock/internal/autonomy"
	"github.com/duckbugio/flock/internal/config"
)

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
// drain) run before the process exits, which a direct os.Exit in main would skip.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	if err := cfg.ValidateTelegram(); err != nil {
		slog.Error("invalid telegram config", "error", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	runner, opts, provider, auth, err := airunner.BuildProvider(cfg, logger)
	if err != nil {
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// A pending device login is deliberately detached from the update that
	// started it, so nothing else would ever end it on shutdown: the polling
	// Codex child would outlive this process as an orphan.
	defer auth.Cancel()

	// Best-effort git startup wiring (identity, default branch, and — when a host
	// + token are set — an inline credential helper that reads the token from the
	// env at call time). Non-fatal: a git setup failure must not crash-loop the bot.
	if err := gitsetup.Apply(ctx, gitsetup.Config{
		Host:        cfg.GitHost,
		Scheme:      cfg.GitScheme,
		AuthorName:  cfg.GitAuthorName,
		AuthorEmail: cfg.GitAuthorEmail,
		HasToken:    cfg.GitToken != "",
	}); err != nil {
		logger.Warn("git setup", "error", err)
	}

	// Wire the context7 MCP docs server (up-to-date, version-specific library/API
	// docs) into every run when enabled. A write failure is non-fatal — the bot
	// runs without it, exactly as before.
	if provider.Name == config.AIBackendClaude {
		servers := map[string]claude.MCPServer{}
		if cfg.EnableContext7 {
			servers["context7"] = claude.MCPServer{URL: claude.Context7URL}
		}
		if cfg.DuckBugMCPEnabled() {
			servers["duckbug"] = claude.MCPServer{URL: cfg.DuckBugMCPURL, BearerToken: cfg.DuckBugMCPToken}
		}
		mcpPath := filepath.Join(cfg.ApprovedDirectory, ".flock-mcp.json")
		if ok, err := claude.WriteMCPConfig(mcpPath, servers); err != nil {
			logger.Warn("write mcp config; running without MCP tools", "error", err)
		} else if ok {
			opts.MCPConfig = mcpPath
			logger.Info("MCP servers enabled",
				"config", mcpPath, "context7", cfg.EnableContext7, "duckbug", cfg.DuckBugMCPEnabled())
		}
	}

	// Per-chat workspaces (isolated /workspace/chat_<id> with a rendered CLAUDE.md
	// + agents) and the dispatcher enforcing parallel-across-chats / serial-within.
	ws := &workspace.Renderer{
		BaseDir:          cfg.ApprovedDirectory,
		TemplatePath:     cfg.TeamTemplatePath,
		AgentsDir:        cfg.TeamAgentsDir,
		SkillsDir:        cfg.TeamSkillsDir,
		PrePRCycles:      cfg.PrePRCycles,
		PrReviewCycles:   cfg.PrReviewCycles,
		EnablePRReview:   cfg.EnablePRReview,
		GitHost:          cfg.GitHost,
		AutoApproveScope: cfg.AutoApproveScopeLevel(),
	}
	// Durable per-chat session store: chatID -> session_id, reloaded on startup so
	// conversations survive a restart and resumed via --resume (plan §4).
	sessions, err := session.Open(cfg.SessionStoreFile())
	if err != nil {
		logger.Error("open session store", "path", cfg.SessionStoreFile(), "error", err)
		return 1
	}

	// Guardrails (plan §9b): a per-user fixed-window rate limiter (in-memory) and a
	// durable per-user cumulative cost accumulator, both enforced on the inbound
	// message path. The limiter is always constructed (it self-disables when its
	// budget/window is non-positive). A cost-store open failure is non-fatal:
	// log-and-continue with a nil store (cost tracking off) like the rest of the
	// best-effort startup wiring — a nil *cost.Store is a safe no-op.
	limiter := ratelimit.New(cfg.RateLimitRequests, cfg.RateLimitWindow())
	costs, err := cost.Open(cfg.CostStoreFile())
	if err != nil {
		logger.Error("open cost store; cost cap disabled", "path", cfg.CostStoreFile(), "error", err)
		costs = nil
	}

	// Durable per-chat interrupted-run store (auto-resume after a deploy/restart):
	// the run path marks a chat at run start and clears it on every clean terminal,
	// and startup re-injects any marker left by a killed run. Like the cost store, a
	// store-open failure is non-fatal — log and continue with a nil/no-op store
	// (auto-resume off) rather than crash-loop the bot.
	pendings, err := pending.Open(cfg.PendingStoreFile())
	if err != nil {
		logger.Error("open pending store; auto-resume disabled", "path", cfg.PendingStoreFile(), "error", err)
		pendings = nil
	}
	logger.Info("guardrails configured",
		"rate_limit_enabled", cfg.RateLimitEnabled(),
		"rate_limit_requests", cfg.RateLimitRequests,
		"rate_limit_window", cfg.RateLimitWindow(),
		"cost_cap_enabled", cfg.CostCapEnabled() && costs != nil,
		"cost_cap_usd", cfg.EffectiveCostCapUSD(),
	)

	disp := dispatch.New(cfg.MaxConcurrentChatRuns)
	// On shutdown (SIGTERM during a deploy), gracefully drain: WAIT for in-flight
	// runs to finish on their own up to the configured drain window (so a run that
	// completes in time delivers its real answer), cancelling only the survivors at
	// the deadline. The drain window is strictly below the Docker stop_grace_period
	// (75s) so the process self-exits before SIGKILL; a survivor's marker, written
	// at run start, drives auto-resume on the next startup.
	if cfg.ShutdownDrainClamped() {
		logger.Warn("SHUTDOWN_DRAIN_SECONDS above cap; clamped",
			"configured_seconds", cfg.ShutdownDrainSeconds,
			"effective", cfg.ShutdownDrain(),
		)
	}
	defer func() {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownDrain())
		defer cancelDrain()
		if err := disp.Shutdown(drainCtx); err != nil {
			logger.Warn("dispatcher drain deadline reached; survivors cancelled", "error", err)
		}
	}()

	// svc, vt and up are created after the bot so they can use it; we use pointer
	// holders because the bot's handlers close over them. vt may stay nil when
	// voice is disabled or its provider fails to configure (text still works); up
	// is always constructed (uploads have no external dependency).
	var svc *chat.Service
	var vt *telegram.VoiceTranscriber
	var up *telegram.Uploader

	guards := chat.GuardConfig{CostCapUSD: cfg.EffectiveCostCapUSD()}

	opts2 := []bot.Option{
		bot.WithDefaultHandler(textHandler(cfg, &svc, &vt, &up, limiter, costs, guards, auth)),
	}

	b, err := bot.New(cfg.TelegramBotToken, opts2...)
	if err != nil {
		logger.Error("create bot", "error", err)
		return 1
	}

	// Voice transcription is optional and OFF by default. A provider
	// misconfiguration (unknown VOICE_PROVIDER, missing key/command) is non-fatal:
	// log a warning and run with voice disabled so text handling is unaffected.
	if cfg.EnableVoiceMessages {
		// One HTTP client shared by the file download and the provider call, so the
		// timeout is a single budget rather than two stacked 60s windows.
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
			vt = telegram.NewVoiceTranscriber(
				telegram.NewBotFileSource(b),
				voiceClient,
				tr,
				0, // default max download size
				logger,
			)
			logger.Info("voice transcription enabled", "provider", cfg.VoiceProvider)
		}
	}

	// Inbound document/photo uploads. Always enabled: the uploader saves files to
	// the per-chat uploads dir (a sibling of the repos, outside every git tree) and
	// reuses the same token-redacting download seam as voice. A dedicated HTTP
	// client gives the (potentially large) file download its own timeout budget.
	uploadClient := &http.Client{Timeout: uploadClientTimeout}
	up = telegram.NewUploader(
		telegram.NewBotFileSource(b),
		uploadClient,
		ws,
		cfg.MaxUploadBytes,
		logger,
	)

	// Outbound file delivery ("outbox"): after each run finishes, sweep the
	// per-chat outbox dir (a sibling of the repos, outside every git tree, reusing
	// the ws Renderer as the OutboxDir resolver like the Uploader) and send each
	// file the run produced as a Telegram document, archiving it under
	// outbox/sent/. Always constructed; the sweep is a no-op when the outbox is
	// empty/absent.
	outbox := chat.NewSweeper(ws, cfg.MaxOutboxBytes, cfg.MaxOutboxFiles, logger)

	// Post-task GitHub star nudge (core/ghstar + core/nudge). GitHub-only and OFF
	// for every other deploy: the gate IS the off switch (StarNudgeEnabled needs
	// github.com + a token + a valid STAR_NUDGE_REPO). When disabled, the config is
	// left zero-valued so the Service no-ops the whole path. A nudge-store open
	// failure is non-fatal, like the rest of the best-effort startup wiring: log
	// and disable the feature rather than crash-loop the bot.
	starNudge := buildStarNudge(cfg, logger)

	// Autonomy loops (durable stores + post-run config). Every store-open failure
	// is non-fatal, matching the rest of the best-effort startup wiring: log and
	// run with that single feature disabled rather than crash-loop the bot.
	postRun := autonomy.Build(cfg, logger)

	svc = chat.New(chat.Config{
		Runner:     runner,
		Transport:  telegram.NewBotChat(b, cfg.EnableRichMessages),
		Dispatcher: disp,
		Workspace:  ws,
		Sessions:   sessions,
		Pending:    pendings,
		Costs:      costs,
		CostCapUSD: cfg.EffectiveCostCapUSD(),
		Outbox:     outbox,
		StarNudge:  starNudge,
		PostRun:    postRun,
		Opts:       opts,
		Timeout:    cfg.ClaudeTimeout(),
		RetryAfter: telegram.RetryAfter,
		// The provider gate on every submit path — a user message and edits of one, a
		// cron fire, the autonomy path (follow-ups, CI events, fix-ups), a poller
		// relay, the restart replay. The adapters block the message path early
		// (before paid work); this is what stops the rest from marching into an
		// unauthenticated CLI and stranding a pending marker per fire.
		Auth:   auth,
		Logger: logger,
	})

	// Background cron scheduler (OFF by default). When enabled, open the durable
	// per-chat job store and start the scheduler loop, firing each due job into the
	// same dispatch lane a user message uses (svc.InjectScheduled, non-blocking +
	// ephemeral, attributed to the job's creator). A store-open failure is non-fatal:
	// log and continue WITHOUT the scheduler (mgr stays nil), like the rest of the
	// best-effort startup wiring. With the flag off no store is opened and no
	// goroutine runs — the path is a true no-op.
	mgr := buildScheduler(ctx, cfg, svc, logger)

	// One-shot follow-ups (the workspace followup/<delay>.md convention): fire
	// each due item as an autonomy-budgeted injected run.
	autonomy.StartFollowups(ctx, svc, postRun, logger)

	// CI watch: poll the git host for CI state on the duck/* branches checked out
	// in the workspaces; a red build is relayed into the owning chat as a fix-up
	// prompt, and (opt-in) a green PR is auto-merged.
	if cfg.CIWatchEnabled() {
		wireCIWatch(ctx, cfg, b, svc, logger)
	}

	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, telegram.CallbackMatch(), bot.MatchTypePrefix, stopHandler(cfg, svc))
	b.RegisterHandler(
		bot.HandlerTypeCallbackQueryData, telegram.StarCallbackPrefix(), bot.MatchTypePrefix, starHandler(cfg, svc),
	)

	// Slash commands. Registered handlers match BEFORE the default text handler.
	// We match via telegram.CommandName (a bot_command entity at offset 0, with
	// any @botname suffix stripped) rather than bot.MatchTypeCommand, because the
	// latter compares the full "new@duck_bot" token and would miss commands
	// addressed with @botname in groups, leaking them to the model. /news still
	// does NOT match (different command name). Each command is gated by the same
	// allow-list as text messages and is NOT mention-gated (a slash command is an
	// explicit address). The set of names handled here is asserted, by test, to equal
	// the canonical chat.ReservedCommands so a new reserved command can never be
	// published in the menu yet leak to the model for lack of a handler.
	handlers := reservedHandlers(cfg, svc, mgr, auth)
	for name, h := range handlers {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}

	// Publish the bot's own command menu from the canonical reserved set (the same
	// source the handlers above route on). Native Claude commands (/loop, /security-review, …)
	// are intentionally NOT listed — they pass through as free text. Best-effort:
	// a failure here only means an empty/stale menu, so log and continue.
	if _, err := b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: reservedBotCommands(auth)}); err != nil {
		logger.Warn("set telegram command menu", "error", err)
	}

	// When PR review is enabled and Gitea polling is configured, poll the bot's
	// unread Pull notifications and relay each new non-self comment on a
	// duck/<chatid>/... PR into the owning chat as a synthetic prompt.
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
					//nolint:contextcheck // detached by design: the run (and its notice) must outlive this poll/replay context.
					svc.Inject(c.ChatID, formatPRComment(c))
				}
			}
		}()
	}

	// Auto-resume after restart: re-inject any run that was still in flight when the
	// process was killed (e.g. SIGKILL after the drain window on a deploy). Each
	// marker's prompt is replayed through the dispatcher (Inject -> Submit), so the
	// MaxConcurrentChatRuns cap throttles a resume storm just like live traffic. The
	// dangling "Working…" anchor is best-effort deleted (non-fatal); the resumed run
	// posts its own fresh anchor. Markers are NOT cleared here — only a clean
	// terminal clears them, so a crash mid-replay just re-resumes next boot. Run
	// SYNCHRONOUSLY before b.Start so resumed work is queued ahead of new live
	// messages; this ordering is intentional.
	resumePending(ctx, b, svc, pendings, logger)

	logger.Info("starting telegram adapter", "username", cfg.TelegramBotUsername)
	b.Start(ctx)
	logger.Info("telegram adapter stopped")
	return 0
}

// textHandler dispatches an allowed user's text message to the run Service,
// which submits it to the Dispatcher. It handles both new messages
// (update.Message) and edits (update.EditedMessage): a new message starts a run
// gated by the group mention-gate, while an edit supersedes the chat's in-flight
// run for that message. Submit returns immediately (the job runs on the chat's
// own goroutine), so the long-poll loop keeps receiving updates (including the
// Stop callback). Per-chat serialization and the global concurrency cap are
// enforced by the Dispatcher.
func textHandler(
	cfg config.Config,
	svc **chat.Service,
	vt **telegram.VoiceTranscriber,
	up **telegram.Uploader,
	limiter *ratelimit.Limiter,
	costs *cost.Store,
	guards chat.GuardConfig,
	auth *codexauth.Manager,
) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		service := *svc
		if service == nil {
			return
		}

		// Edited messages: an edit supersedes the in-flight run for that message.
		// Voice and media uploads apply only to new messages, never edits, so
		// neither the transcriber nor the uploader is threaded here.
		if edited := update.EditedMessage; edited != nil {
			deps := messageDeps{cfg: cfg, service: service, limiter: limiter, costs: costs, guards: guards, auth: auth}
			handleMessage(ctx, deps, b, edited, true)
			return
		}
		if msg := update.Message; msg != nil {
			deps := messageDeps{
				cfg: cfg, service: service, vt: *vt, up: *up,
				limiter: limiter, costs: costs, guards: guards, auth: auth,
			}
			handleMessage(ctx, deps, b, msg, false)
		}
	}
}

// messageSubmitter is the narrow seam the inbound message path uses to hand a
// prompt to the run Service: the four submit entry points (new/edit, text/media).
// *chat.Service satisfies it. Depending on this interface rather than the concrete
// Service lets the end-to-end message-path test (e.g. the native-command
// pass-through) drive handleMessage with a recording fake, mirroring the VK
// receiver's Service seam. The reserved-command handlers keep taking the concrete
// *chat.Service (they use Stop/StopChat/NewSession), so this seam is scoped to the
// pass-through path it tests.
type messageSubmitter interface {
	Handle(ctx context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string)
	HandleEdit(ctx context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string)
	HandleMedia(
		ctx context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string, images []agent.ImageInput,
	)
	HandleEditMedia(
		ctx context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string, images []agent.ImageInput,
	)
}

// messageDeps bundles the long-lived dependencies a single message handler needs,
// keeping the per-message entry points to a small, stable argument list. Edited
// messages leave vt/up nil (voice and media uploads apply only to new messages).
type messageDeps struct {
	cfg     config.Config
	service messageSubmitter
	vt      *telegram.VoiceTranscriber
	up      *telegram.Uploader
	limiter *ratelimit.Limiter
	costs   *cost.Store
	guards  chat.GuardConfig
	// auth blocks runs while the Codex backend has no completed sign-in. Nil (and
	// a nil *Manager) means "never blocks".
	auth *codexauth.Manager
}

// handleMessage applies the allow-list and the group mention-gate to one message
// (new or edited) and routes it to the Service. The mention-gate decision is the
// pure chat.ShouldHandle; this function only extracts its transport-agnostic
// inputs from the Telegram message (and parses the @username entities the
// adapter is responsible for).
func handleMessage(ctx context.Context, deps messageDeps, b *bot.Bot, msg *models.Message, isEdit bool) {
	cfg, service, vt, up := deps.cfg, deps.service, deps.vt, deps.up
	limiter, costs, guards := deps.limiter, deps.costs, deps.guards
	if msg.From == nil {
		return
	}
	if !cfg.IsAllowed(msg.From.ID) {
		slog.Debug("ignoring message from disallowed user", "user_id", msg.From.ID)
		return
	}

	// Telegram carries the text either as Text or, for media, as Caption; the
	// gate treats both the same way the Python adapter does.
	rawText := msg.Text
	entities := msg.Entities
	if rawText == "" && msg.Caption != "" {
		rawText = msg.Caption
		entities = msg.CaptionEntities
	}

	// A voice message we may transcribe: no text/caption, a voice payload, and a
	// configured transcriber. The gate decision for a voice note does not depend on
	// the transcript (it carries no @mention), so we evaluate it on the empty text
	// FIRST and only transcribe once the message is accepted — never spending a paid
	// transcription call (or a multi-MiB download) on a message we'd discard.
	isVoice := rawText == "" && msg.Voice != nil && vt != nil

	// A document or photo upload. Like voice, the download happens only AFTER the
	// gate + guards accept the message (gate first, pay later), so a discarded media
	// message never triggers a download. The gate decision uses the caption text the
	// same way as a text message (already folded into rawText/entities above).
	isDocument := !isVoice && up != nil && msg.Document != nil
	isPhoto := !isVoice && !isDocument && up != nil && len(msg.Photo) > 0

	in := chat.GateInput{
		IsGroup:        isGroup(msg.Chat.Type),
		Text:           rawText,
		Entities:       toGateEntities(entities),
		ReplyToBot:     replyToBot(b, msg),
		BotUsername:    cfg.TelegramBotUsername,
		BotUserID:      botID(b),
		RequireMention: cfg.RequireGroupMention,
	}
	handle, cleaned := chat.ShouldHandle(in)
	if !handle {
		slog.Debug("group message not addressed to bot — ignoring", "chat_id", msg.Chat.ID)
		return
	}

	// Guardrails (plan §9b): once the message is accepted for handling but BEFORE
	// any paid work (voice transcription or the Claude run), apply the per-user
	// rate limit and cumulative cost cap. A denied message spends no transcription
	// call and is never submitted to the dispatcher; the user gets a single
	// professional notice. Disallowed users already returned above, so the guards
	// only ever see allow-listed senders.
	if allow, reason := chat.CheckGuards(limiter, costs, guards, msg.From.ID); !allow {
		slog.Debug("guardrail denied message", "chat_id", msg.Chat.ID, "user_id", msg.From.ID, "reason", reason)
		sendCommandReply(ctx, b, msg.Chat.ID, reason)
		return
	}

	// Codex with no completed sign-in cannot run anything. Say so here — after the
	// mention gate (so an unaddressed group message stays silent) and after the rate
	// limit (an unauthorized deploy in a busy group would otherwise answer every
	// single message, unthrottled), but before any paid work: the guards spend
	// nothing, while transcription and downloads do. /login is a reserved command
	// routed elsewhere, so it stays reachable while this gate is closed. core/chat
	// gates the run itself; this is the early, user-facing half.
	if notice, blocked := deps.auth.BlockedNotice(); blocked {
		slog.Debug("codex unauthorized — blocking run", "chat_id", msg.Chat.ID)
		sendCommandReply(ctx, b, msg.Chat.ID, notice)
		return
	}

	text := strings.TrimSpace(cleaned)

	// Normalize the group form of a forwarded slash command so an unknown command
	// reaches Claude as a bare command: "/loop@duck_bot 5m" -> "/loop 5m". Reserved
	// commands (/start /help /new /stop /schedule) are routed to their own handlers and never
	// reach here, so this only ever touches Claude pass-through commands; it is a
	// no-op when there is no leading "/command@botname".
	text = telegram.StripCommandMention(text, cfg.TelegramBotUsername)

	// The chat/message passed the gate; for a voice note, transcribe now and use the
	// transcript as the text. The download runs on this update's own goroutine
	// (go-telegram dispatches each update concurrently), so the blocking call does
	// not stall the poll loop. An empty/unintelligible transcript gets explicit
	// feedback rather than being silently dropped.
	if isVoice {
		transcript, err := vt.Transcribe(ctx, msg.Voice.FileID)
		if err != nil {
			slog.Warn("voice transcription failed", "chat_id", msg.Chat.ID, "error", err)
			sendVoiceNotice(ctx, b, msg.Chat.ID, "Sorry, I couldn't transcribe that voice message.")
			return
		}
		text = strings.TrimSpace(transcript)
		if text == "" {
			sendVoiceNotice(ctx, b, msg.Chat.ID, "Sorry, I couldn't make out that voice message.")
			return
		}
	}

	// Document/photo uploads (gate + guards already passed). The file is saved to
	// the per-chat uploads dir — a sibling of the cloned repos, outside every git
	// tree — so it can never enter a commit/PR. Oversize/undownloadable yields one
	// notice (never a silent drop). These submit directly and return, so they sit
	// before the final empty-text drop below.
	if isDocument {
		handleDocument(ctx, b, up, service, msg, isEdit)
		return
	}
	if isPhoto {
		handlePhoto(ctx, b, up, service, msg, isEdit)
		return
	}

	if text == "" {
		return
	}

	// Fold any quoted/replied-to original into the prompt so the run sees the
	// context the user is referring to, not just their new text (voice is covered
	// too: its transcript became `text` above). QuotedPrompt is a strict no-op when
	// there is no quote, so a non-reply message stays byte-for-byte identical.
	qAuthor, qText := quotedContext(b, msg)
	text = chat.QuotedPrompt(qAuthor, qText, text)

	// Submit hands the work to the chat's own goroutine and is effectively
	// non-blocking until that chat's queue buffer fills (then it applies
	// back-pressure); during shutdown the job is cleanly dropped. Either way no
	// extra goroutine is needed here.
	if isEdit {
		service.HandleEdit(ctx, chatIDStr(msg.Chat.ID), msg.From.ID, msgIDStr(msg.ID), text)
		return
	}
	service.Handle(ctx, chatIDStr(msg.Chat.ID), msg.From.ID, msgIDStr(msg.ID), text)
}

// chatIDStr renders Telegram's int64 chat id as the transport-neutral string id
// the core/chat Service uses. msgIDStr does the same for the int message id. The
// adapter converts them back at its boundary (see adapters/telegram.botChat).
func chatIDStr(id int64) string { return strconv.FormatInt(id, 10) }

func msgIDStr(id int) string { return strconv.Itoa(id) }

// handleDocument downloads + saves an uploaded document and submits a run whose
// prompt references the saved path plus the user's caption. The download runs on
// this update's own goroutine, so the blocking call does not stall the poll loop.
// Oversize/undownloadable yields one notice (never a silent drop).
func handleDocument(
	ctx context.Context, b *bot.Bot, up *telegram.Uploader, service messageSubmitter, msg *models.Message, isEdit bool,
) {
	doc := msg.Document
	// Reject an oversize file before downloading when Telegram reports its size.
	if doc.FileSize > 0 && doc.FileSize > up.MaxBytes() {
		sendNotice(ctx, b, msg.Chat.ID, "Sorry, that file is too large for me to handle.")
		return
	}
	savedPath, err := up.Save(ctx, msg.Chat.ID, doc.FileID, doc.FileName)
	if err != nil {
		slog.Warn("save document failed", "chat_id", msg.Chat.ID, "error", err)
		sendNotice(ctx, b, msg.Chat.ID, "Sorry, I couldn't save that file.")
		return
	}
	prompt := chat.DocumentPrompt(savedPath, msg.Caption)
	qAuthor, qText := quotedContext(b, msg)
	prompt = chat.QuotedPrompt(qAuthor, qText, prompt)
	submitMedia(ctx, service, msg, prompt, nil, isEdit)
}

// handlePhoto downloads + saves the largest size of an uploaded photo and submits
// a vision run: the prompt references the saved path plus caption, and the image
// is attached as a content block so Claude can see it. The photo is ALWAYS saved
// to uploads too, giving a no-commit-safe on-disk copy even in vision mode.
func handlePhoto(
	ctx context.Context, b *bot.Bot, up *telegram.Uploader, service messageSubmitter, msg *models.Message, isEdit bool,
) {
	photo := largestPhoto(msg.Photo)
	if photo == nil {
		return
	}
	if photo.FileSize > 0 && int64(photo.FileSize) > up.MaxBytes() {
		sendNotice(ctx, b, msg.Chat.ID, "Sorry, that image is too large for me to handle.")
		return
	}
	// Telegram photo sizes carry no filename; synthesize a .jpg name. Telegram
	// always re-encodes message.photo to JPEG (unlike documents, which preserve
	// their type and take the path-only document route), so the synthesized
	// extension and the image/jpeg vision media-type are correct — don't "fix" this
	// to sniff the type. The saved file and its vision media-type line up.
	savedPath, err := up.Save(ctx, msg.Chat.ID, photo.FileID, "photo.jpg")
	if err != nil {
		slog.Warn("save photo failed", "chat_id", msg.Chat.ID, "error", err)
		sendNotice(ctx, b, msg.Chat.ID, "Sorry, I couldn't save that image.")
		return
	}
	prompt := chat.PhotoPrompt(savedPath, msg.Caption)
	qAuthor, qText := quotedContext(b, msg)
	prompt = chat.QuotedPrompt(qAuthor, qText, prompt)
	images, err := chat.LoadPhotoImage(savedPath)
	if err != nil {
		// Vision attach failed (e.g. unreadable file); fall back to a path-only run
		// so the user still gets a response referencing the saved image.
		slog.Warn("load photo image failed; path-only", "chat_id", msg.Chat.ID, "error", err)
		images = nil
	}
	submitMedia(ctx, service, msg, prompt, images, isEdit)
}

// submitMedia routes a media-derived run through the Service, mirroring the
// new-vs-edit split of the text path. Images are non-nil only for a vision run.
func submitMedia(
	ctx context.Context, service messageSubmitter, msg *models.Message, prompt string, images []agent.ImageInput, isEdit bool,
) {
	if isEdit {
		service.HandleEditMedia(ctx, chatIDStr(msg.Chat.ID), msg.From.ID, msgIDStr(msg.ID), prompt, images)
		return
	}
	service.HandleMedia(ctx, chatIDStr(msg.Chat.ID), msg.From.ID, msgIDStr(msg.ID), prompt, images)
}

// largestPhoto returns the highest-resolution PhotoSize from a Telegram photo
// set, preferring the largest FileSize and falling back to Width*Height when the
// size is unknown. Returns nil for an empty set.
func largestPhoto(sizes []models.PhotoSize) *models.PhotoSize {
	var best *models.PhotoSize
	var bestScore int
	for i := range sizes {
		s := &sizes[i]
		score := s.FileSize
		if score == 0 {
			score = s.Width * s.Height
		}
		if best == nil || score > bestScore {
			best = s
			bestScore = score
		}
	}
	return best
}

// isGroup reports whether a chat type is a group or supergroup (where the
// mention-gate applies); private chats and channels are not gated as groups.
func isGroup(t models.ChatType) bool {
	return t == models.ChatTypeGroup || t == models.ChatTypeSupergroup
}

// replyToBot reports whether msg is a reply to one of the bot's own messages.
func replyToBot(b *bot.Bot, msg *models.Message) bool {
	reply := msg.ReplyToMessage
	if reply == nil || reply.From == nil {
		return false
	}
	id := botID(b)
	return id != 0 && reply.From.ID == id
}

// botID returns the bot's own Telegram user id, or 0 if it can't be determined.
// The go-telegram library exposes the id parsed from the token; deriving it from
// the token avoids an extra getMe round-trip on every message.
func botID(b *bot.Bot) int64 {
	return b.ID()
}

// assistantAuthorLabel names the bot's own earlier message when it is the quoted
// original, so the folded context reads as coming from the assistant rather than a
// human participant.
const assistantAuthorLabel = "the assistant"

// quotedContext extracts the quoted/replied-to original from a message so it can be
// folded into the prompt (via chat.QuotedPrompt). It returns an empty text when the
// message is not a reply/quote, which keeps a normal message byte-for-byte
// unchanged. The quoted text prefers the exact highlighted portion (msg.Quote.Text),
// then the replied-to message's text, then its caption, then a generic "[media]"
// placeholder for any reply with no usable text — so the reference is never silently
// dropped. The author is derived from the replied-to message's sender (empty for a
// quote-only selection with no sender), labeled as the assistant when it is the
// bot's own message.
func quotedContext(b *bot.Bot, msg *models.Message) (author, text string) {
	if msg == nil {
		return "", ""
	}
	reply := msg.ReplyToMessage
	switch {
	case msg.Quote != nil && strings.TrimSpace(msg.Quote.Text) != "":
		text = msg.Quote.Text
	case reply != nil && reply.Text != "":
		text = reply.Text
	case reply != nil && reply.Caption != "":
		text = reply.Caption
	case reply != nil:
		// A reply to a message with no text or caption — a photo, file, voice, poll,
		// location, and so on: keep a generic reference so the quote is never dropped.
		text = "[media]"
	}
	if reply != nil {
		author = quotedAuthorLabel(b, reply.From)
	}
	return author, text
}

// quotedAuthorLabel derives a short author label for a replied-to message's sender.
// Only the bot's OWN message (the same identity check as replyToBot: from.ID ==
// botID) is labeled as the assistant; any other sender — a human or a third-party
// bot — yields "FirstName (@username)" (or whichever part is present), and an unknown
// sender yields an empty label.
func quotedAuthorLabel(b *bot.Bot, from *models.User) string {
	if from == nil {
		return ""
	}
	if id := botID(b); id != 0 && from.ID == id {
		return assistantAuthorLabel
	}
	name := strings.TrimSpace(from.FirstName)
	username := strings.TrimSpace(from.Username)
	switch {
	case name != "" && username != "":
		return name + " (@" + username + ")"
	case name != "":
		return name
	case username != "":
		return "@" + username
	default:
		return ""
	}
}

// toGateEntities maps Telegram message entities to the transport-agnostic
// chat.Entity used by the gate, keeping only the mention kinds it cares
// about.
func toGateEntities(entities []models.MessageEntity) []chat.Entity {
	if len(entities) == 0 {
		return nil
	}
	out := make([]chat.Entity, 0, len(entities))
	for _, e := range entities {
		switch e.Type {
		case models.MessageEntityTypeMention:
			out = append(out, chat.Entity{
				Type:   chat.EntityMention,
				Offset: e.Offset,
				Length: e.Length,
			})
		case models.MessageEntityTypeTextMention:
			var uid int64
			if e.User != nil {
				uid = e.User.ID
			}
			out = append(out, chat.Entity{
				Type:   chat.EntityTextMention,
				Offset: e.Offset,
				Length: e.Length,
				UserID: uid,
			})
		default:
			// Other entity kinds (bold, code, URLs, …) are irrelevant to the gate.
		}
	}
	return out
}

// stopHandler cancels the in-flight run for the pressed Stop button and
// acknowledges the callback so Telegram clears the button's spinner. Run IDs are
// sequential and guessable, so we gate the callback on the same allow-list as
// text messages: a disallowed sender cannot cancel another user's run.
func stopHandler(cfg config.Config, svc *chat.Service) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		cq := update.CallbackQuery
		if cq == nil {
			return
		}
		// CallbackQuery.From is a value (not a pointer); a zero ID means no real
		// sender, so treat it as disallowed.
		if cq.From.ID == 0 || !cfg.IsAllowed(cq.From.ID) {
			slog.Debug("ignoring stop callback from disallowed user", "user_id", cq.From.ID)
			if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Not available.",
			}); err != nil {
				slog.Debug("answer disallowed stop callback", "error", err)
			}
			return
		}
		runID := telegram.StopRunID(cq.Data)
		stopped := false
		if runID != "" {
			stopped = svc.Stop(runID)
		}
		text := "Stopping…"
		if !stopped {
			text = "Run already finished."
		}
		if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cq.ID,
			Text:            text,
		}); err != nil {
			slog.Debug("answer stop callback", "error", err)
		}
	}
}

// buildStarNudge assembles the post-task star-nudge config. It returns a
// zero-valued (disabled) config when the feature is off (non-GitHub deploy,
// missing token, or bad STAR_NUDGE_REPO) or when the durable flag store cannot be
// opened — both leave the Service path inert. A GitHub client and the nudge store
// are only constructed when the feature is fully enabled.
func buildStarNudge(cfg config.Config, logger *slog.Logger) chat.StarNudgeConfig {
	if !cfg.StarNudgeEnabled() {
		return chat.StarNudgeConfig{}
	}
	owner, repo, ok := cfg.StarNudgeRepoParts()
	if !ok {
		// Should not happen (StarNudgeEnabled already validated the shape), but stay
		// fail-safe rather than trust the invariant.
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

// starHandler handles a press of the star-nudge confirm button: it stars the repo
// from the deployment account (Service.StarPress), answers the callback with a
// short toast, and on success edits the nudge message into the confirmation
// (clearing the button). It is gated by the same allow-list as the Stop callback
// so a disallowed sender cannot trigger the star. Every failure is soft: the run
// is never affected.
func starHandler(cfg config.Config, svc *chat.Service) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		cq := update.CallbackQuery
		if cq == nil {
			return
		}
		if cq.From.ID == 0 || !cfg.IsAllowed(cq.From.ID) {
			slog.Debug("ignoring star callback from disallowed user", "user_id", cq.From.ID)
			if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cq.ID,
				Text:            "Not available.",
			}); err != nil {
				slog.Debug("answer disallowed star callback", "error", err)
			}
			return
		}

		// StarPress detaches its own bounded context so the star completes even if
		// this short callback ctx is already done.
		//nolint:contextcheck // intentionally detached; see StarPress.
		toast, ok := svc.StarPress()
		if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cq.ID,
			Text:            toast,
		}); err != nil {
			slog.Debug("answer star callback", "error", err)
		}
		// On success, edit the nudge message into the confirmation and drop the
		// button. Best-effort: a failed edit never undoes the star.
		if ok && cq.Message.Message != nil {
			if _, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    cq.Message.Message.Chat.ID,
				MessageID: cq.Message.Message.ID,
				Text:      chat.StarDoneText(),
			}); err != nil {
				slog.Debug("edit star nudge message", "error", err)
			}
		}
	}
}

// reservedBotCommands maps the canonical reserved set (core/chat) to the Telegram
// command-menu model published via SetMyCommands. It is the single place the menu
// is built, so the menu can never drift from the commands the handlers route on.
func reservedBotCommands(auth *codexauth.Manager) []models.BotCommand {
	cmds := make([]models.BotCommand, 0, len(chat.ReservedCommands))
	for _, c := range chat.ReservedCommands {
		// /login is meaningless on a deployment with no interactive sign-in (Claude,
		// an API-key backend, Codex billing): it would sit in the menu only to answer
		// that it is not needed. The handler stays registered either way, so a user
		// who types it still gets that answer.
		if c.Name == loginCommand && !auth.Applicable() {
			continue
		}
		cmds = append(cmds, models.BotCommand{Command: c.Name, Description: c.Description})
	}
	return cmds
}

// reservedHandlers maps each reserved command name to the handler that serves it.
// It is the single source the registration loop drives, keyed by the same bare
// command words as chat.ReservedCommands. Keying both the menu (reservedBotCommands)
// and the handlers off the canonical set means a new reserved command cannot be
// published in the menu yet leak to the model for lack of a handler — a guard test
// asserts this map's key set equals chat.ReservedCommands. Adding a reserved command
// is therefore a two-line change here (plus the canonical entry); forgetting the
// handler fails the test rather than silently leaking the command to Claude.
func reservedHandlers(
	cfg config.Config, svc *chat.Service, sched *schedule.Manager, auth *codexauth.Manager,
) map[string]bot.HandlerFunc {
	return map[string]bot.HandlerFunc{
		"start":      startHandler(cfg, auth),
		"help":       helpHandler(cfg, auth),
		"new":        newHandler(cfg, svc),
		"stop":       stopCommandHandler(cfg, svc),
		"schedule":   scheduleHandler(cfg, sched),
		"goal":       goalHandler(cfg, svc),
		loginCommand: loginHandler(cfg, auth),
	}
}

// loginHandler serves /login: it starts (or reports, or cancels) the Codex
// device-code sign-in. The command is deliberately available even while the
// deployment is unauthorized — it is the only way out of that state — and, like
// every reserved handler, is allow-list gated and never starts a run.
//
// Whether a one-time code may be printed here is decided by codexauth from the
// Private flag, not by this handler inspecting the arguments: /login status also
// re-shows a pending code, so an argument-based guard would let it straight past.
//
// The device flow outlives this handler: Dispatch returns an immediate reply and
// keeps working in the background, delivering the link, the one-time code, and
// the final verdict through the notify callback. That callback therefore builds
// its OWN context: the update's ctx is cancelled as soon as the handler returns,
// minutes before a user finishes signing in, and sending on it would fail.
func loginHandler(cfg config.Config, auth *codexauth.Manager) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		sub := codexauth.Subscriber{
			ID:      chatIDStr(chatID),
			Private: update.Message.Chat.Type == models.ChatTypePrivate,
			Notify: func(text string) {
				sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexauth.NotifyTimeout)
				defer cancel()
				sendCommandReply(sendCtx, b, chatID, text)
			},
		}
		// An empty reply means Dispatch already delivered it through sub, in order.
		if reply := auth.Dispatch(ctx, commandArgs(update.Message.Text), sub); reply != "" {
			sendCommandReply(ctx, b, chatID, reply)
		}
	}
}

// loginCommand is the reserved command name the sign-in flow is published under.
const loginCommand = "login"

// commandMatch routes a message whose leading bot command is name, tolerating
// the @botname suffix Telegram adds in groups (/new@duck_bot). It only routes;
// the allow-list gate lives in each handler via commandSender.
func commandMatch(name string) bot.MatchFunc {
	return func(u *models.Update) bool {
		return u != nil && u.Message != nil && telegram.CommandName(u.Message) == name
	}
}

// commandSender reports whether the slash-command update comes from an allowed
// user and, if so, returns the chat id to reply to. A disallowed (or sender-less)
// command gets the SAME silent-ignore as a disallowed text message: no session
// mutation, no run, no reply, no info leak. The bool is false when the command
// must be ignored.
func commandSender(cfg config.Config, msg *models.Message) (int64, bool) {
	if msg == nil || msg.From == nil {
		return 0, false
	}
	if !cfg.IsAllowed(msg.From.ID) {
		slog.Debug("ignoring command from disallowed user", "user_id", msg.From.ID)
		return 0, false
	}
	return msg.Chat.ID, true
}

// startHandler replies to /start from an allowed user with the static welcome
// text (a short greeting + the usage help). Like helpHandler it is gated by the
// allow-list, is not mention-gated, and never touches the dispatcher or session
// store (no Claude run) — without it /start would fall through to the text
// handler and be forwarded to the model as a prompt.
func startHandler(cfg config.Config, auth *codexauth.Manager) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		sendCommandReply(ctx, b, chatID, telegram.WelcomeText(chat.LoginVisibilityFor(auth.Applicable())))
	}
}

// helpHandler replies to /help from an allowed user with the static usage text.
// It never touches the dispatcher or session store (no Claude run).
func helpHandler(cfg config.Config, auth *codexauth.Manager) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		sendCommandReply(ctx, b, chatID, telegram.HelpText(chat.LoginVisibilityFor(auth.Applicable())))
	}
}

// newHandler resets the calling chat's session (so the next message starts fresh
// with no --resume) and confirms. A reset of a chat with no stored session is a
// harmless no-op that still confirms.
func newHandler(cfg config.Config, svc *chat.Service) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		if err := svc.NewSession(chatIDStr(chatID)); err != nil {
			slog.Error("reset session", "chat_id", chatID, "error", err)
		}
		sendCommandReply(ctx, b, chatID, "Started a fresh session. Your next message begins a new conversation.")
	}
}

// stopCommandHandler cancels the chat's in-flight run via the same primitive as
// the inline Stop button and acknowledges. When nothing is running it says so.
func stopCommandHandler(cfg config.Config, svc *chat.Service) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		text := "Nothing to stop."
		if svc.StopChat(chatIDStr(chatID)) {
			text = "Stopping the current run."
		}
		sendCommandReply(ctx, b, chatID, text)
	}
}

// scheduleDisabledText is the reply when /schedule is used but the scheduler is
// turned off (the default). It tells the operator exactly which flag enables it.
const scheduleDisabledText = "Scheduler is disabled. Set ENABLE_SCHEDULER=true to enable it."

// scheduleHandler serves /schedule from an allowed user. When the scheduler is
// disabled (mgr == nil) it replies with the disabled notice; otherwise it strips
// the leading /schedule token and hands the remaining args, the chat id, and the
// sender's user id to the transport-neutral Manager.Dispatch, replying with its
// result. Like the other reserved handlers it is allow-list gated (via
// commandSender) and never starts a Claude run.
func scheduleHandler(cfg config.Config, mgr *schedule.Manager) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		if mgr == nil {
			sendCommandReply(ctx, b, chatID, scheduleDisabledText)
			return
		}
		args := commandArgs(update.Message.Text)
		reply := mgr.Dispatch(chatIDStr(chatID), update.Message.From.ID, args)
		sendCommandReply(ctx, b, chatID, reply)
	}
}

// commandArgs returns the text following the leading "/command[@botname]" token of
// a slash-command message, i.e. the arguments. It drops the first whitespace-
// delimited token (the command itself, including any @botname suffix) and trims
// the surrounding whitespace, so "/schedule add x" yields "add x" and a bare
// "/schedule" yields "".
func commandArgs(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// buildScheduler opens the durable cron-job store and starts the background
// scheduler when ENABLE_SCHEDULER is set, returning the Manager the /schedule
// handler dispatches to. It returns nil when the feature is disabled OR when the
// store cannot be opened (non-fatal: log and run without the scheduler), so the
// handler replies with the disabled notice rather than crashing. The scheduler
// fires each due job via svc.InjectScheduled — the same serialized per-chat lane a
// normal message uses, but non-blocking, ephemeral, and re-validated against the
// allow-list at fire time.
func buildScheduler(ctx context.Context, cfg config.Config, svc *chat.Service, logger *slog.Logger) *schedule.Manager {
	if !cfg.SchedulerEnabled() {
		return nil
	}
	store, err := schedule.Open(cfg.ScheduleStoreFile())
	if err != nil {
		logger.Error("open schedule store; scheduler disabled", "path", cfg.ScheduleStoreFile(), "error", err)
		return nil
	}
	// Fire each due job directly via InjectScheduled: it is non-blocking (drops the
	// fire when the chat's lane is full) and ephemeral (no auto-resume marker), so
	// the single scheduler goroutine is never stalled by a busy lane and a frequent
	// cron cannot accumulate detached goroutines or durable markers. The creator is
	// re-validated against the live allow-list at fire time (cfg.IsAllowed) so a
	// de-listed user's stored jobs stop running and are pruned.
	mgr := schedule.NewManager(store, svc.InjectScheduled, cfg.IsAllowed, time.Now, logger)
	go func() {
		if err := mgr.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("scheduler stopped", "error", err)
		}
	}()
	logger.Info("scheduler enabled", "store", cfg.ScheduleStoreFile())
	return mgr
}

// sendCommandReply posts a short command confirmation. Delivery failures are
// non-fatal and only logged at debug.
func sendCommandReply(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		slog.Debug("send command reply", "error", err)
	}
}

// resumePending re-submits every interrupted run recorded in the pending store so
// a run killed mid-flight (or merely queued behind a running run) on a
// deploy/restart continues WITHOUT the user re-sending their message. The store
// holds a per-chat FIFO queue; for each chat its markers are replayed IN ORDER.
// For each marker it best-effort DELETES the dangling "Working…" anchor (only
// when an anchor id was captured; non-fatal on failure) so the frozen progress
// message disappears, then re-submits via ResumePending, which REUSES the
// marker's id so the resumed run clears the SAME on-disk marker on its clean
// terminal — never a duplicate. The resumed run posts its own fresh "Working…"
// anchor as usual, so the user immediately sees live activity. The
// dispatcher's concurrency cap throttles the replay. Order is best-effort: a
// concurrent same-chat submit may interleave enqueue vs dispatch order — the
// guarantee is no-loss, not strict order. Markers are intentionally left in place
// (cleared by id only on a clean terminal), so a crash mid-replay re-resumes only
// the not-yet-cleared markers on the next boot, and never double-runs a marker
// within one boot (each is resumed once). A nil store (open failed) yields an
// empty set, so this is a safe no-op.
func resumePending(
	ctx context.Context, b *bot.Bot, svc *chat.Service, store *pending.FileStore, logger *slog.Logger,
) {
	markers := store.All()
	total := 0
	for _, q := range markers {
		total += len(q)
	}
	if total == 0 {
		return
	}
	logger.Info("resuming interrupted runs after restart", "count", total)
	for chatID, queue := range markers {
		id, err := strconv.ParseInt(chatID, 10, 64)
		if err != nil {
			logger.Warn("pending: bad chat id; skipping resume", "chat_id", chatID)
			continue
		}
		for _, m := range queue {
			// Best-effort: delete the frozen "Working…" anchor so the stale progress
			// message disappears; the resumed run posts its own fresh anchor below. A
			// failed/absent delete never blocks the resume.
			if m.AnchorMsgID != "" {
				if anchor, perr := strconv.Atoi(m.AnchorMsgID); perr == nil {
					if _, derr := b.DeleteMessage(ctx, &bot.DeleteMessageParams{
						ChatID:    id,
						MessageID: anchor,
					}); derr != nil {
						logger.Debug("delete dangling anchor on resume", "chat_id", chatID, "error", derr)
					}
				}
			}
			//nolint:contextcheck // detached by design: the run (and its notice) must outlive this poll/replay context.
			svc.ResumePending(chatID, m)
		}
	}
}

// formatPRComment builds a neutral English prompt relaying a polled PR review
// comment into a chat. It is an engineering artifact (no duck flavor) and never
// includes any token. The branch is implied by the chat id segment.
func formatPRComment(c poller.PRComment) string {
	return fmt.Sprintf(
		"A reviewer left a comment on pull request #%d in %s (branch duck/%s/…).\n\n"+
			"Comment by @%s:\n%s\n\n"+
			"Please address this PR review comment following the Phase 2 review workflow.",
		c.PRIndex, c.Repo, c.ChatID, c.Author, c.Body,
	)
}

// sendVoiceNotice posts a short user-facing notice about a voice message (a
// transcription failure or an empty transcript). Delivery failures are non-fatal
// and only logged at debug.
func sendVoiceNotice(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		slog.Debug("send voice notice", "error", err)
	}
}

// sendNotice posts a short user-facing notice (e.g. an oversize/undownloadable
// upload). Delivery failures are non-fatal and only logged at debug.
func sendNotice(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		slog.Debug("send notice", "error", err)
	}
}
