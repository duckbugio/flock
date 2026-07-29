//nolint:testpackage // intentionally whitebox to test unexported telegram command handlers
package telegram

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/chat"
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
		{"uppercase command lowercased", botCmd("/Stop", 5), "stop"},
		{"all-caps command lowercased", botCmd("/STOP", 5), "stop"},
		{"mixed-case command lowercased", botCmd("/New please", 4), "new"},
		{"uppercase command with @botname", botCmd("/STOP@duck_bot now", 14), "stop"},
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

// TestStripCommandMention asserts the group-form normalizer strips ONLY a leading
// "/command@botUsername" suffix that addresses THIS bot, so a forwarded native
// Claude command reaches the model as a bare command. It must leave a plain
// command, a different bot's mention, a mid-text "@bot", and empty/"/" inputs
// untouched, and match the bot username case-insensitively (Telegram may
// lowercase it).
func TestStripCommandMention(t *testing.T) {
	const bot = "duck_bot"
	tests := []struct {
		name        string
		text        string
		botUsername string
		want        string
	}{
		{"no suffix unchanged", "/loop 5m", bot, "/loop 5m"},
		{"group form stripped", "/loop@duck_bot 5m", bot, "/loop 5m"},
		{"group form no args", "/loop@duck_bot", bot, "/loop"},
		{"different bot untouched", "/loop@other_bot 5m", bot, "/loop@other_bot 5m"},
		{"case-insensitive bot match", "/loop@Duck_Bot 5m", bot, "/loop 5m"},
		{"lowercased delivery", "/loop@duck_bot 5m", "Duck_Bot", "/loop 5m"},
		{"mid-text mention untouched", "ping @duck_bot now", bot, "ping @duck_bot now"},
		{"non-command with mention untouched", "hi @duck_bot", bot, "hi @duck_bot"},
		{"empty unchanged", "", bot, ""},
		{"lone slash unchanged", "/", bot, "/"},
		{"empty bot username is no-op", "/loop@duck_bot 5m", "", "/loop@duck_bot 5m"},
		{"empty mention not our bot", "/loop@ 5m", bot, "/loop@ 5m"},
		{"tab-delimited args preserved", "/loop@duck_bot\t5m", bot, "/loop\t5m"},
		{"newline-delimited args preserved", "/loop@duck_bot\nmore", bot, "/loop\nmore"},
		{"carriage-return-delimited args preserved", "/loop@duck_bot\r5m", bot, "/loop\r5m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripCommandMention(tc.text, tc.botUsername); got != tc.want {
				t.Fatalf("StripCommandMention(%q, %q) = %q, want %q", tc.text, tc.botUsername, got, tc.want)
			}
		})
	}
}

// TestHelpTextListsCommands asserts the static /help payload exists and lists all
// three slash commands — AC1. /help is a pure reply (it has no Service method, so
// it can never submit a run); the constant IS the entire payload the handler
// sends.
func TestHelpTextListsCommands(t *testing.T) {
	if HelpText(chat.WithLogin) == "" {
		t.Fatal("HelpText(chat.WithLogin) is empty")
	}
	for _, cmd := range []string{"/help", "/new", "/stop"} {
		if !strings.Contains(HelpText(chat.WithLogin), cmd) {
			t.Fatalf("HelpText(chat.WithLogin) does not mention %q:\n%s", cmd, HelpText(chat.WithLogin))
		}
	}
}
