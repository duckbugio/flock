package main

import (
	"context"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/internal/config"
)

// quoteTestBot builds a *bot.Bot whose id is 123 (parsed from the token) without a
// getMe round-trip, so quotedContext's bot-identity check has a stable id.
func quoteTestBot(t *testing.T) *bot.Bot {
	t.Helper()
	b, err := bot.New("123:ABC", bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	return b
}

// TestHandleMessageFoldsQuotedReply (text path): a reply carrying the user's new
// text folds the replied-to original into the submitted prompt, keeping both parts.
func TestHandleMessageFoldsQuotedReply(t *testing.T) {
	sub := &recordingSubmitter{}
	b := quoteTestBot(t)
	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:   2,
		From: &models.User{ID: 10},
		Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate},
		Text: "Про это",
		ReplyToMessage: &models.Message{
			From: &models.User{ID: 77, FirstName: "Ivan"},
			Text: "Проверил функционал Git Runners",
		},
	}

	deps := messageDeps{cfg: cfg, service: sub, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, b, msg, false)

	if len(sub.seen()) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(sub.seen()))
	}
	got := sub.seen()[0]
	if !strings.Contains(got, "Проверил функционал Git Runners") {
		t.Errorf("prompt %q missing the quoted original", got)
	}
	if !strings.Contains(got, "Про это") {
		t.Errorf("prompt %q missing the user's new text", got)
	}
	if !strings.Contains(got, "Ivan") {
		t.Errorf("prompt %q missing the quoted author", got)
	}
}

// TestHandleMessageLabelsBotQuoteAsAssistant (text path): replying to the bot's own
// message labels the quoted author as the assistant.
func TestHandleMessageLabelsBotQuoteAsAssistant(t *testing.T) {
	sub := &recordingSubmitter{}
	b := quoteTestBot(t)
	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:   3,
		From: &models.User{ID: 10},
		Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate},
		Text: "expand on that",
		ReplyToMessage: &models.Message{
			// The bot's OWN message: its sender id equals the bot's id (123, parsed
			// from the "123:ABC" token), which is what marks it as the assistant.
			From: &models.User{ID: 123, IsBot: true, FirstName: "Flock"},
			Text: "Here is the summary.",
		},
	}

	deps := messageDeps{cfg: cfg, service: sub, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, b, msg, false)

	if len(sub.seen()) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(sub.seen()))
	}
	got := sub.seen()[0]
	if !strings.Contains(got, assistantAuthorLabel) {
		t.Errorf("prompt %q should label the bot's quoted message as %q", got, assistantAuthorLabel)
	}
	if strings.Contains(got, "Flock") {
		t.Errorf("prompt %q leaked the bot's display name instead of the assistant label", got)
	}
}

// TestHandleMessageThirdPartyBotNotAssistant (text path): replying to a DIFFERENT
// bot's message is attributed to that bot's name, never "the assistant" — only our
// own bot's messages (matched by id) are the assistant.
func TestHandleMessageThirdPartyBotNotAssistant(t *testing.T) {
	sub := &recordingSubmitter{}
	b := quoteTestBot(t)
	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:   8,
		From: &models.User{ID: 10},
		Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate},
		Text: "what did it say",
		ReplyToMessage: &models.Message{
			From: &models.User{ID: 555001, IsBot: true, FirstName: "OtherBot", Username: "other_bot"},
			Text: "some third-party output",
		},
	}

	deps := messageDeps{cfg: cfg, service: sub, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, b, msg, false)

	if len(sub.seen()) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(sub.seen()))
	}
	got := sub.seen()[0]
	if strings.Contains(got, assistantAuthorLabel) {
		t.Errorf("prompt %q mislabeled a third-party bot as the assistant", got)
	}
	if !strings.Contains(got, "OtherBot") {
		t.Errorf("prompt %q should attribute the quote to the third-party bot's name", got)
	}
}

// TestHandleMessageNonReplyUnchanged (text path): a plain message with no reply/
// quote is submitted byte-for-byte unchanged (the QuotedPrompt no-op).
func TestHandleMessageNonReplyUnchanged(t *testing.T) {
	sub := &recordingSubmitter{}
	b := quoteTestBot(t)
	cfg := config.Config{AllowedUsers: []int64{10}}
	const userText = "just a normal message"
	msg := &models.Message{
		ID:   4,
		From: &models.User{ID: 10},
		Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate},
		Text: userText,
	}

	deps := messageDeps{cfg: cfg, service: sub, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, b, msg, false)

	if len(sub.seen()) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(sub.seen()))
	}
	if sub.seen()[0] != userText {
		t.Errorf("non-reply prompt = %q, want it byte-for-byte unchanged (%q)", sub.seen()[0], userText)
	}
}

// TestHandleMessageQuoteWinsOverReplyText (text path): when the user highlights an
// exact portion (msg.Quote), that selection is folded — not the whole replied-to
// text. This is Telegram's native "quote" feature and the highest-preference branch.
func TestHandleMessageQuoteWinsOverReplyText(t *testing.T) {
	sub := &recordingSubmitter{}
	b := quoteTestBot(t)
	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:    6,
		From:  &models.User{ID: 10},
		Chat:  models.Chat{ID: 555, Type: models.ChatTypePrivate},
		Text:  "clarify this part",
		Quote: &models.TextQuote{Text: "the highlighted fragment"},
		ReplyToMessage: &models.Message{
			From: &models.User{ID: 77, FirstName: "Ivan"},
			Text: "the full original message body",
		},
	}

	deps := messageDeps{cfg: cfg, service: sub, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, b, msg, false)

	if len(sub.seen()) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(sub.seen()))
	}
	got := sub.seen()[0]
	if !strings.Contains(got, "the highlighted fragment") {
		t.Errorf("prompt %q missing the highlighted quote selection", got)
	}
	if strings.Contains(got, "the full original message body") {
		t.Errorf("prompt %q folded the full replied-to text instead of the highlighted selection", got)
	}
}

// TestHandleMessageMediaOnlyReplyPlaceholder (text path): a reply to a message with
// no text or caption (e.g. a photo) still yields a non-empty [media] reference rather
// than silently dropping the quote.
func TestHandleMessageMediaOnlyReplyPlaceholder(t *testing.T) {
	sub := &recordingSubmitter{}
	b := quoteTestBot(t)
	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:   7,
		From: &models.User{ID: 10},
		Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate},
		Text: "про это",
		ReplyToMessage: &models.Message{
			From:  &models.User{ID: 77, FirstName: "Ivan"},
			Photo: []models.PhotoSize{{FileID: "x", Width: 100, Height: 100}},
		},
	}

	deps := messageDeps{cfg: cfg, service: sub, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, b, msg, false)

	if len(sub.seen()) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(sub.seen()))
	}
	got := sub.seen()[0]
	if !strings.Contains(got, "[media]") {
		t.Errorf("prompt %q missing the [media] placeholder for a media-only reply", got)
	}
	if !strings.Contains(got, "про это") {
		t.Errorf("prompt %q missing the user's new text", got)
	}
}

// TestHandlePhotoWithQuoteIncludesQuote (media path): a quote+caption+photo message
// carries the quoted original AND the caption AND the saved path, with the vision
// image still attached. Reuses the media harness (real Service + Uploader).
func TestHandlePhotoWithQuoteIncludesQuote(t *testing.T) {
	h := newMediaHarness(t)
	defer h.srvCleanup()

	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:      5,
		From:    &models.User{ID: 10},
		Chat:    models.Chat{ID: 888, Type: models.ChatTypePrivate},
		Caption: "what is this?",
		Photo: []models.PhotoSize{
			{FileID: "large", Width: 1280, Height: 1280, FileSize: 50000},
		},
		ReplyToMessage: &models.Message{
			From: &models.User{ID: 77, FirstName: "Ivan"},
			Text: "the earlier note",
		},
	}

	deps := messageDeps{cfg: cfg, service: h.svc, up: h.up, guards: chat.GuardConfig{}}
	handleMessage(context.Background(), deps, h.b, msg, false)

	waitFor(t, func() bool {
		prompts, _ := h.runner.seen()
		return len(prompts) == 1
	})
	prompts, imgs := h.runner.seen()
	if !strings.Contains(prompts[0], "the earlier note") {
		t.Errorf("photo+quote prompt %q missing the quoted original", prompts[0])
	}
	if !strings.Contains(prompts[0], "what is this?") {
		t.Errorf("photo+quote prompt %q missing the caption", prompts[0])
	}
	if !strings.Contains(prompts[0], "uploads") {
		t.Errorf("photo+quote prompt %q missing the saved uploads path", prompts[0])
	}
	if imgs[0] != 1 {
		t.Errorf("photo+quote run carried %d image(s), want 1 (vision still attached)", imgs[0])
	}
}
