package telegram

import (
	"strings"

	"github.com/go-telegram/bot/models"
)

// CommandName returns the slash command addressed at the start of msg, without
// any @botname suffix, or "" when msg is not a bot command. Telegram inserts an
// @botname suffix when a command is tapped in a group (e.g. /new@duck_bot), and
// go-telegram/bot's MatchTypeCommand compares the FULL "new@duck_bot" token, so
// it would miss the command in groups; stripping the suffix here makes /new and
// /new@duck_bot equivalent. Only a bot_command entity at offset 0 counts, so a
// "/new" appearing mid-message is not treated as a command.
func CommandName(msg *models.Message) string {
	if msg == nil {
		return ""
	}
	for _, e := range msg.Entities {
		if e.Type != models.MessageEntityTypeBotCommand || e.Offset != 0 {
			continue
		}
		end := e.Offset + e.Length
		// Commands (and the @botname suffix) are ASCII, so byte indexing the
		// entity span is safe and mirrors how the library itself slices.
		if end > len(msg.Text) || e.Offset+1 > end {
			return ""
		}
		tok := msg.Text[e.Offset+1 : end] // drop the leading '/'
		if i := strings.IndexByte(tok, '@'); i >= 0 {
			tok = tok[:i]
		}
		return tok
	}
	return ""
}

// HelpText is the static usage message replied to an allowed user who sends
// /help. It lists the slash commands the adapter understands. It is an
// engineering artifact (professional English, no duck flavor) and never reaches
// the Claude Runner.
const HelpText = "DuckFlock Telegram assistant — available commands:\n\n" +
	"/help — show this message\n" +
	"/new — start a fresh session (forget the current conversation)\n" +
	"/stop — stop the run currently in progress\n\n" +
	"Send any other message to run it through the assistant."

// WelcomeText is the static usage message replied to an allowed user who sends
// /start. It is a short greeting prepended to the command help so a brand-new
// user immediately sees what the bot does and how to use it. Like HelpText it is
// an engineering artifact (plain English, no duck flavor) and never reaches the
// Claude Runner — the duck greeting comes from the model on a real message.
const WelcomeText = "Hi! I'm the DuckFlock assistant.\n\n" + HelpText
