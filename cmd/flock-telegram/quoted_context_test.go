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
			From: &models.User{ID: 999, IsBot: true, FirstName: "Flock"},
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
