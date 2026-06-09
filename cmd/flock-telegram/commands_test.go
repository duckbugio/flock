package main

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/adapters/telegram"
	"github.com/duckbugio/flock/internal/config"
)

// TestCommandSenderAllowList is the AC4 gate: only an allowed sender's slash
// command is acted on; a disallowed (or sender-less) command is silently ignored
// — the same treatment a disallowed text message gets, with no info leak.
func TestCommandSenderAllowList(t *testing.T) {
	cfg := config.Config{AllowedUsers: []int64{10, 20}}

	tests := []struct {
		name       string
		msg        *models.Message
		wantChatID int64
		wantOK     bool
	}{
		{
			name:       "allowed user",
			msg:        &models.Message{From: &models.User{ID: 10}, Chat: models.Chat{ID: 555}},
			wantChatID: 555,
			wantOK:     true,
		},
		{
			name:   "disallowed user ignored",
			msg:    &models.Message{From: &models.User{ID: 99}, Chat: models.Chat{ID: 555}},
			wantOK: false,
		},
		{
			name:   "sender-less message ignored",
			msg:    &models.Message{Chat: models.Chat{ID: 555}},
			wantOK: false,
		},
		{
			name:   "nil message ignored",
			msg:    nil,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chatID, ok := commandSender(cfg, tt.msg)
			if ok != tt.wantOK {
				t.Fatalf("commandSender ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && chatID != tt.wantChatID {
				t.Fatalf("commandSender chatID = %d, want %d", chatID, tt.wantChatID)
			}
		})
	}
}

// botCommandUpdate builds an Update carrying a slash command, mirroring what
// Telegram sends (a bot_command entity at offset 0 spanning "/name[@botname]").
func botCommandUpdate(text string, cmdLen int) *models.Update {
	return &models.Update{Message: &models.Message{
		Text:     text,
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: cmdLen}},
	}}
}

// TestStartCommandRouted is the /start routing guard: /start must be matched as a command
// (so it is handled by the start handler and never reaches the text/model path),
// tolerating the group @botname suffix exactly like the other slash commands.
// Without a /start handler the message falls through to the default text handler
// and is forwarded to the model, which improvises a confusing reply.
func TestStartCommandRouted(t *testing.T) {
	match := commandMatch("start")
	tests := []struct {
		name string
		u    *models.Update
		want bool
	}{
		{"bare /start", botCommandUpdate("/start", 6), true},
		{"group form /start@duck_bot", botCommandUpdate("/start@duck_bot", 15), true},
		{"/start with args", botCommandUpdate("/start now", 6), true},
		{"different command /stop", botCommandUpdate("/stop", 5), false},
		{"nil update", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := match(tt.u); got != tt.want {
				t.Fatalf("commandMatch(start)(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestStartWelcomeText asserts the static /start reply is the welcome text: a
// short greeting prepended to the usage help, plain English with no duck flavor.
func TestStartWelcomeText(t *testing.T) {
	if !strings.HasPrefix(telegram.WelcomeText, "Hi!") {
		t.Fatalf("WelcomeText should open with a greeting, got %q", telegram.WelcomeText)
	}
	if !strings.Contains(telegram.WelcomeText, telegram.HelpText) {
		t.Fatalf("WelcomeText should include the usage help (HelpText)")
	}
}

// TestCommandMatchToleratesBotnameSuffix is the routing guard: a command handler
// must match both /new and the group form /new@duck_bot (Telegram auto-inserts
// the @botname suffix in groups), while NOT matching a different command or a
// nil update. Without this the group form would fall through to the text handler
// and be forwarded to the model.
func TestCommandMatchToleratesBotnameSuffix(t *testing.T) {
	match := commandMatch("new")
	tests := []struct {
		name string
		u    *models.Update
		want bool
	}{
		{"bare /new", botCommandUpdate("/new", 4), true},
		{"group form /new@duck_bot", botCommandUpdate("/new@duck_bot", 13), true},
		{"/new with args", botCommandUpdate("/new please", 4), true},
		{"different command /news", botCommandUpdate("/news", 5), false},
		{"nil update", nil, false},
		{"update without message", &models.Update{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := match(tt.u); got != tt.want {
				t.Fatalf("commandMatch(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
