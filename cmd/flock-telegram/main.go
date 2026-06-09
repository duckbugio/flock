// Command flock-telegram is the Go Telegram adapter for flock. Each allowed
// user's message is dispatched to that chat's isolated workspace and run through
// the core/claude Runner; the event stream renders to one live "Working… (Ns)"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/adapters/telegram"
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
// drain) run before the process exits, which a direct os.Exit in main would skip.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

	runner := claude.New(cfg.ClaudeBin)
	// Workdir is resolved per chat at run time (workspace.Renderer); only the
	// model/turns/env are constant across runs.
	opts := claude.Options{
		Model:    cfg.ClaudeModel,
		MaxTurns: cfg.ClaudeMaxTurns,
		Env:      claudeEnv(cfg),
	}

	// Per-chat workspaces (isolated /workspace/chat_<id> with a rendered CLAUDE.md
	// + agents) and the dispatcher enforcing parallel-across-chats / serial-within.
	ws := &workspace.Renderer{
		BaseDir:        cfg.ApprovedDirectory,
		TemplatePath:   cfg.TeamTemplatePath,
		AgentsDir:      cfg.TeamAgentsDir,
		PrePRCycles:    cfg.PrePRCycles,
		PrReviewCycles: cfg.PrReviewCycles,
		EnablePRReview: cfg.EnablePRReview,
		GitHost:        cfg.GitHost,
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
	logger.Info("guardrails configured",
		"rate_limit_enabled", cfg.RateLimitEnabled(),
		"rate_limit_requests", cfg.RateLimitRequests,
		"rate_limit_window", cfg.RateLimitWindow(),
		"cost_cap_enabled", cfg.CostCapEnabled() && costs != nil,
		"cost_cap_usd", cfg.ClaudeMaxCostPerUser,
	)

	disp := dispatch.New(cfg.MaxConcurrentChatRuns)
	// On shutdown, give in-flight runs a bounded chance to deliver their final
	// message before exit (graceful drain) rather than dropping them with the
	// non-waiting Close(). Close() remains available for callers that don't drain.
	defer func() {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer cancelDrain()
		if err := disp.Shutdown(drainCtx); err != nil {
			logger.Warn("dispatcher drain timed out", "error", err)
		}
	}()

	// svc, vt and up are created after the bot so they can use it; we use pointer
	// holders because the bot's handlers close over them. vt may stay nil when
	// voice is disabled or its provider fails to configure (text still works); up
	// is always constructed (uploads have no external dependency).
	var svc *chat.Service
	var vt *telegram.VoiceTranscriber
	var up *telegram.Uploader

	guards := chat.GuardConfig{CostCapUSD: cfg.ClaudeMaxCostPerUser}

	opts2 := []bot.Option{
		bot.WithDefaultHandler(textHandler(cfg, &svc, &vt, &up, limiter, costs, guards)),
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

	svc = chat.New(chat.Config{
		Runner:     runner,
		Transport:  telegram.NewBotChat(b),
		Dispatcher: disp,
		Workspace:  ws,
		Sessions:   sessions,
		Costs:      costs,
		Outbox:     outbox,
		StarNudge:  starNudge,
		Opts:       opts,
		Timeout:    cfg.ClaudeTimeout(),
		RetryAfter: telegram.RetryAfter,
		Logger:     logger,
	})

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
	// explicit address).
	b.RegisterHandlerMatchFunc(commandMatch("help"), helpHandler(cfg))
	b.RegisterHandlerMatchFunc(commandMatch("new"), newHandler(cfg, svc))
	b.RegisterHandlerMatchFunc(commandMatch("stop"), stopCommandHandler(cfg, svc))

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
					svc.Inject(c.ChatID, formatPRComment(c))
				}
			}
		}()
	}

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
			deps := messageDeps{cfg: cfg, service: service, limiter: limiter, costs: costs, guards: guards}
			handleMessage(ctx, deps, b, edited, true)
			return
		}
		if msg := update.Message; msg != nil {
			deps := messageDeps{cfg: cfg, service: service, vt: *vt, up: *up, limiter: limiter, costs: costs, guards: guards}
			handleMessage(ctx, deps, b, msg, false)
		}
	}
}

// messageDeps bundles the long-lived dependencies a single message handler needs,
// keeping the per-message entry points to a small, stable argument list. Edited
// messages leave vt/up nil (voice and media uploads apply only to new messages).
type messageDeps struct {
	cfg     config.Config
	service *chat.Service
	vt      *telegram.VoiceTranscriber
	up      *telegram.Uploader
	limiter *ratelimit.Limiter
	costs   *cost.Store
	guards  chat.GuardConfig
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

	text := strings.TrimSpace(cleaned)

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
	ctx context.Context, b *bot.Bot, up *telegram.Uploader, service *chat.Service, msg *models.Message, isEdit bool,
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
	submitMedia(ctx, service, msg, prompt, nil, isEdit)
}

// handlePhoto downloads + saves the largest size of an uploaded photo and submits
// a vision run: the prompt references the saved path plus caption, and the image
// is attached as a content block so Claude can see it. The photo is ALWAYS saved
// to uploads too, giving a no-commit-safe on-disk copy even in vision mode.
func handlePhoto(
	ctx context.Context, b *bot.Bot, up *telegram.Uploader, service *chat.Service, msg *models.Message, isEdit bool,
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
	ctx context.Context, service *chat.Service, msg *models.Message, prompt string, images []claude.ImageInput, isEdit bool,
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

// helpHandler replies to /help from an allowed user with the static usage text.
// It never touches the dispatcher or session store (no Claude run).
func helpHandler(cfg config.Config) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID, ok := commandSender(cfg, update.Message)
		if !ok {
			return
		}
		sendCommandReply(ctx, b, chatID, telegram.HelpText)
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

// sendCommandReply posts a short command confirmation. Delivery failures are
// non-fatal and only logged at debug.
func sendCommandReply(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		slog.Debug("send command reply", "error", err)
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

// claudeEnv derives the child process environment for the claude CLI: the
// process environment plus the configured auth token, so the CLI authenticates
// the same way the deploy provides it.
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
