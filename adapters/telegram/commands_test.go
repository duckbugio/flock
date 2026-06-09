//nolint:testpackage // intentionally whitebox to test unexported telegram command handlers
package telegram

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

// botCmd builds a message whose leading token is a bot_command entity, the shape
// Telegram produces for a slash command (the entity spans "/name[@botname]").
func botCmd(text string, cmdLen int) *models.Message {
	return &models.Message{
		Text:     text,
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: cmdLen}},
	}
}

// TestCommandName asserts the command parser strips the @botname suffix Telegram
// adds in groups (so /new and /new@duck_bot are equivalent), ignores arguments
// and non-commands, and does not treat a "/new" appearing mid-message as a
// command — the routing fix that keeps group commands from leaking to the model.
func TestCommandName(t *testing.T) {
	tests := []struct {
		name string
		msg  *models.Message
		want string
	}{
		{"bare command", botCmd("/new", 4), "new"},
		{"command with @botname (group form)", botCmd("/new@duck_bot", 13), "new"},
		{"command with args", botCmd("/new please", 4), "new"},
		{"command with @botname and args", botCmd("/stop@duck_bot now", 14), "stop"},
		{"different command not confused", botCmd("/news", 5), "news"},
		{"plain text is not a command", &models.Message{Text: "hello there"}, ""},
		{"mid-text slash is not a command", &models.Message{
			Text:     "say /new",
			Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 4, Length: 4}},
		}, ""},
		{"nil message", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommandName(tc.msg); got != tc.want {
				t.Fatalf("CommandName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHelpTextListsCommands asserts the static /help payload exists and lists all
// three slash commands — AC1. /help is a pure reply (it has no Service method, so
// it can never submit a run); the constant IS the entire payload the handler
// sends.
func TestHelpTextListsCommands(t *testing.T) {
	if HelpText == "" {
		t.Fatal("HelpText is empty")
	}
	for _, cmd := range []string{"/help", "/new", "/stop"} {
		if !strings.Contains(HelpText, cmd) {
			t.Fatalf("HelpText does not mention %q:\n%s", cmd, HelpText)
		}
	}
}
