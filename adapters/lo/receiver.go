package lo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/schedule"
)

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
	Service        Service
	Client         *Client
	Transport      *Transport
	Username       string
	IsAllowed      func(int64) bool
	Guards         func(int64) (bool, string)
	RequireMention bool
	Scheduler      *schedule.Manager
	Logger         *slog.Logger
}

// Receiver serially admits updates while the shared dispatcher runs chats concurrently.
type Receiver struct{ cfg ReceiverConfig }

// NewReceiver defaults to denying users unless an allow-list is wired.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	if cfg.IsAllowed == nil {
		cfg.IsAllowed = func(int64) bool { return false }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Receiver{cfg: cfg}
}

// Run uses normal positive-offset acknowledgement, retaining queued updates on startup.
// Only one polling process should run for a bot token; 409 is returned to the operator.
func (r *Receiver) Run(ctx context.Context) error {
	var offset int64
	for ctx.Err() == nil {
		updates, err := r.cfg.Client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if fatalAPIError(err) {
				return err
			}
			delay, ok := RetryAfter(err)
			if !ok {
				delay = time.Second
			}
			r.cfg.Logger.Warn("lo: polling failed; retrying", "error", err)
			if !wait(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
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

// HandleUpdate gates every command and message before invoking the shared service.
func (r *Receiver) HandleUpdate(ctx context.Context, update Update) {
	msg := update.Message
	if msg == nil || msg.From == nil || msg.From.IsBot || msg.From.ID <= 0 || msg.ID <= 0 ||
		msg.Chat.ID == 0 || !r.cfg.IsAllowed(msg.From.ID) {
		return
	}
	if msg.Chat.Type != "private" && msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	cleaned, addressed := addressedText(text, r.cfg.Username)
	if !addressed {
		return
	}
	if msg.Chat.Type != "private" && r.cfg.RequireMention && cleaned == text {
		return
	}
	text = cleaned
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	name, args := command(text)
	if chat.IsReservedCommand(name) {
		r.reserved(ctx, chatID, msg.From.ID, name, args)
		return
	}
	if hasMedia(msg) {
		r.notify(ctx, chatID, "Attachments are not supported yet. Please send the relevant text or a repository path.")
		return
	}
	if text == "" {
		return
	}
	if r.cfg.Guards != nil {
		if allowed, reason := r.cfg.Guards(msg.From.ID); !allowed {
			r.notify(ctx, chatID, reason)
			return
		}
	}
	if msg.Reply != nil {
		quoted := msg.Reply.Text
		if quoted == "" {
			quoted = msg.Reply.Caption
		}
		author := ""
		if msg.Reply.From != nil {
			author = msg.Reply.From.Username
		}
		text = chat.QuotedPrompt(author, quoted, text)
	}
	r.cfg.Service.Handle(ctx, chatID, msg.From.ID, strconv.FormatInt(msg.ID, 10), text)
}

func hasMedia(msg *Message) bool {
	for _, raw := range []json.RawMessage{
		msg.Photo, msg.Document, msg.Voice, msg.Audio, msg.Video,
		msg.Animation, msg.Sticker, msg.VideoNote,
	} {
		value := strings.TrimSpace(string(raw))
		if value != "" && value != "null" && value != "[]" {
			return true
		}
	}
	return false
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
