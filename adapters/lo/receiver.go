package lo

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/schedule"
)

const privateChatType = "private"

// Service is the transport-neutral inbound seam; no Telegram handlers are reused.
type Service interface {
	Handle(ctx context.Context, chatID chat.ChatID, userID int64, messageID chat.MessageID, text string)
	StopChat(chatID chat.ChatID) bool
	NewSession(chatID chat.ChatID) error
	ArmGoal(chatID chat.ChatID, criterion string) (goal.Goal, bool)
	GoalStatus(chatID chat.ChatID) (goal.Goal, bool)
	DisarmGoal(chatID chat.ChatID) bool
}

// ReceiverConfig owns the LO allow-list separately from Telegram/VK identities.
type ReceiverConfig struct {
	Service   Service
	Client    *Client
	Transport *Transport
	Username  string
	BotID     int64
	// ConflictDelay overrides retry timing; nonpositive values use the production default.
	ConflictDelay  time.Duration
	IsAllowed      func(int64) bool
	Guards         func(int64) (bool, string)
	RequireMention bool
	Scheduler      *schedule.Manager
	// Uploads saves inbound photos into the per-chat uploads directory so the agent can
	// open them. Nil disables the download and every attachment gets the same explanatory
	// notice it got before — the adapter must still run where no workspace is wired.
	Uploads *Uploader
	Logger  *slog.Logger
}

// Receiver serially admits updates while the shared dispatcher runs chats concurrently.
type Receiver struct{ cfg ReceiverConfig }

// NewReceiver defaults to denying users unless an allow-list is wired.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	if cfg.ConflictDelay <= 0 {
		cfg.ConflictDelay = pollingConflictDelay
	}
	if cfg.IsAllowed == nil {
		cfg.IsAllowed = func(int64) bool { return false }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Receiver{cfg: cfg}
}

// Run uses normal positive-offset acknowledgement, retaining queued updates on startup.
// Only one polling process should run for a bot token; persistent conflicts stop polling.
func (r *Receiver) Run(ctx context.Context) error {
	var offset int64
	conflicts := 0
	for ctx.Err() == nil {
		updates, err := r.cfg.Client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if r.stopPolling(err, &conflicts) {
				return err
			}
			delay := r.pollingRetryDelay(err)
			r.cfg.Logger.Warn("lo: polling failed; retrying", "error", err)
			if !wait(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
		conflicts = 0
		previousOffset := offset
		sort.SliceStable(updates, func(i, j int) bool { return updates[i].ID < updates[j].ID })
		for _, update := range updates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if update.ID < offset || update.ID < 0 {
				continue
			}
			if update.ID == math.MaxInt64 {
				return errors.New("LO update ID exceeds acknowledgement range")
			}
			r.HandleUpdate(ctx, update)
			offset = update.ID + 1
		}
		// Empty or stale-only immediate responses must not become a busy polling loop.
		if offset == previousOffset && !wait(ctx, time.Second) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

const (
	maxPollingConflicts  = 5
	pollingConflictDelay = 10 * time.Second
)

// stopPolling tolerates an old in-flight poll after restart, but bounds conflicts
// with four ten-second waits (40s > our 30s long poll), while bounding contention.
func (r *Receiver) stopPolling(err error, conflicts *int) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == 409 {
		*conflicts++
		r.cfg.Logger.Error("lo: polling conflict", "attempt", *conflicts)
		return *conflicts >= maxPollingConflicts
	}
	*conflicts = 0
	return fatalAPIError(err)
}

// HandleUpdate gates every command and message before invoking the shared service.
func (r *Receiver) HandleUpdate(ctx context.Context, update Update) {
	msg := update.Message
	if msg == nil || msg.From == nil || msg.From.IsBot || msg.From.ID <= 0 || msg.ID <= 0 ||
		msg.Chat.ID == 0 || !r.cfg.IsAllowed(msg.From.ID) {
		return
	}
	if msg.Chat.Type != privateChatType && msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	cleaned, addressed := addressedText(text, r.cfg.Username, msg.Chat.Type != privateChatType)
	if !addressed {
		return
	}
	name, args := command(cleaned)
	replyToBot := r.cfg.BotID > 0 && msg.Reply != nil && msg.Reply.From != nil && msg.Reply.From.ID == r.cfg.BotID
	if msg.Chat.Type != privateChatType && r.cfg.RequireMention && cleaned == text && !replyToBot && !chat.IsReservedCommand(name) {
		return
	}
	text = cleaned
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	if chat.IsReservedCommand(name) {
		r.reserved(ctx, chatID, msg.From.ID, name, args)
		return
	}
	// Guards run BEFORE any attachment work. A rate limit or a spent cost cap must be paid
	// with a refusal, not with a download: doing the network call and the disk write first
	// means a capped user still costs bandwidth and storage on every message they send.
	if hasMedia(msg) || text != "" {
		if r.cfg.Guards != nil {
			if allowed, reason := r.cfg.Guards(msg.From.ID); !allowed {
				r.notify(ctx, chatID, reason)
				return
			}
		}
	}
	// Attachments are answered BEFORE the empty-text check: a photo with no caption is a
	// complete request ("look at this"), and dropping it silently is what made the bot
	// look broken.
	attached, note := r.attachments(ctx, msg, chatID)
	if note != "" {
		// The file the request rests on never reached the agent, so the caption must not be
		// answered on its own: "review this file" without the file invites a confident answer
		// about nothing. One notice, no run.
		r.notify(ctx, chatID, note)
		return
	}
	if text == "" && attached == "" {
		return
	}
	text = replyPrompt(msg, replyToBot, text)
	if attached != "" {
		text = strings.TrimSpace(text + "\n" + attached)
	}
	r.cfg.Service.Handle(ctx, chatID, msg.From.ID, strconv.FormatInt(msg.ID, 10), text)
}

func hasMedia(msg *Message) bool {
	if len(msg.Photo) > 0 {
		return true
	}
	for _, att := range []*Attachment{
		msg.Document, msg.Voice, msg.Audio, msg.Video,
		msg.Animation, msg.Sticker, msg.VideoNote,
	} {
		if att != nil {
			return true
		}
	}
	return false
}

// unservedKinds names the attachments LO accepts from a user but will not hand to a bot,
// in the order a message is inspected. Each carries its own sentence: "attachments are not
// supported" taught users nothing about which of their files the bot could actually read,
// and photos now CAN be read.
//
//nolint:gochecknoglobals // A fixed table, read-only, kept beside the function that uses it.
var unservedKinds = []struct {
	get    func(*Message) *Attachment
	notice string
}{
	{
		get: func(m *Message) *Attachment { return m.Document },
		notice: "I cannot open documents: LO's bot API does not serve their bytes yet. " +
			"Paste the text, or point me at the file in a repository.",
	},
	{
		get:    func(m *Message) *Attachment { return m.Voice },
		notice: "I cannot listen to voice messages on LO yet. Please send the request as text.",
	},
	{
		get:    func(m *Message) *Attachment { return m.VideoNote },
		notice: "I cannot open video notes on LO yet. Please send the request as text.",
	},
	{
		get: func(m *Message) *Attachment { return m.Video },
		notice: "I cannot download video on LO: the platform hands bots a reference, not the bytes. " +
			"Describe what it shows, or send a screenshot as a photo.",
	},
	{
		get: func(m *Message) *Attachment { return m.Audio },
		notice: "I cannot download audio on LO: the platform hands bots a reference, not the bytes. " +
			"Please send the request as text.",
	},
	{
		get:    func(m *Message) *Attachment { return m.Animation },
		notice: "I cannot open animations on LO yet. A still screenshot sent as a photo works.",
	},
	{
		get:    func(m *Message) *Attachment { return m.Sticker },
		notice: "Stickers carry nothing I can act on. Please send the request as text.",
	},
}

// attachments turns a message's files into (prompt addition, user notice). Both may be
// empty. A photo is downloaded into the chat's uploads directory and the agent is told the
// path; everything else gets the sentence that says what this platform withholds.
//
// A download failure is a NOTICE, never a dropped message: the user watched their file
// arrive and deserves to know it did not reach the agent.
func (r *Receiver) attachments(ctx context.Context, msg *Message, chatID string) (prompt, notice string) {
	if size, ok := LargestPhoto(msg.Photo); ok {
		if r.cfg.Uploads == nil {
			return "", "I cannot read images in this deployment: no uploads directory is configured."
		}
		path, err := r.cfg.Uploads.Save(ctx, chatID, size.FileID, photoFileName(msg.ID))
		switch {
		case errors.Is(err, ErrUploadTooLarge):
			return "", "That image is too large for me to open. Please send a smaller one."
		case errors.Is(err, ErrNoBytes):
			return "", "LO did not hand me the bytes of that image, so I cannot open it."
		case err != nil:
			r.cfg.Logger.Warn("lo: photo download failed", "error", err)
			return "", "I could not download that image. Please try sending it again."
		}
		return "[The user attached an image. It is saved at " + path + " — open it to see what they mean.]", ""
	}
	for _, kind := range unservedKinds {
		if kind.get(msg) != nil {
			return "", kind.notice
		}
	}
	return "", ""
}

func (r *Receiver) notify(ctx context.Context, chatID, text string) {
	for _, chunk := range chat.ChunkFencedSize(text, r.cfg.Transport.Capabilities().MaxMessageRunes) {
		if _, err := r.cfg.Transport.Send(ctx, chatID, chunk, "", false); err != nil {
			r.cfg.Logger.Warn("lo: notice delivery failed", "error", err)
			return
		}
	}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// pollingRetryDelay keeps a faulty server hint from suspending reception for hours.
// Match the delivery loop's maximum wait while retaining shorter server hints.
func (r *Receiver) pollingRetryDelay(err error) time.Duration {
	const maxWait = 30 * time.Second
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusConflict {
		return r.cfg.ConflictDelay
	}
	delay, ok := RetryAfter(err)
	if !ok {
		return time.Second
	}
	return min(delay, maxWait)
}

func replyPrompt(msg *Message, replyToBot bool, text string) string {
	if msg.Reply == nil {
		return text
	}
	quoted := msg.Reply.Text
	if quoted == "" {
		quoted = msg.Reply.Caption
	}
	if hasMedia(msg.Reply) {
		quoted += "\n[Quoted attachment is unavailable to this bot; ask the user for its contents if needed.]"
	}
	if quoted == "" {
		quoted = "[media]"
	}
	author := ""
	if msg.Reply.From != nil {
		author = msg.Reply.From.Username
	}
	if replyToBot {
		author = chat.AssistantAuthorLabel
	}
	return chat.QuotedPrompt(author, quoted, text)
}
