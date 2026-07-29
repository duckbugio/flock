package telegram

import (
	"strings"

	"github.com/go-telegram/bot/models"
)

// CommandName returns the slash command addressed at the start of msg,
// lowercased and without any @botname suffix, or "" when msg is not a bot
// command. Telegram inserts an @botname suffix when a command is tapped in a
// group (e.g. /new@duck_bot), and go-telegram/bot's MatchTypeCommand compares
// the FULL "new@duck_bot" token, so it would miss the command in groups;
// stripping the suffix here makes /new and /new@duck_bot equivalent. The token
// is lowercased because Telegram preserves the case the user typed for the
// command word (only the @botname suffix is lowercased by clients), so /Stop
// arrives as "Stop"; lowercasing keeps reserved-command routing case-insensitive
// (matching VK and chat.IsReservedCommand) instead of leaking /Stop, /New, … to
// the model as free text. Only a bot_command entity at offset 0 counts, so a
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
		return strings.ToLower(tok)
	}
	return ""
}

// StripCommandMention normalizes the group form of a forwarded slash command so
// Claude receives it as a bare command. In a group, Telegram appends "@botname"
// to a tapped command (e.g. "/loop@duck_bot 5m"); this strips ONLY that
// "@botUsername" suffix from the leading "/command" token, yielding "/loop 5m".
//
// It is intentionally conservative — a pure string transform with no entity
// awareness — and a no-op unless ALL of these hold:
//   - botUsername is non-empty,
//   - text begins with "/",
//   - the leading whitespace-delimited token contains "@<botUsername>" as its
//     suffix (case-insensitive: Telegram may lowercase the username, so we match
//     either casing).
//
// It never touches a non-leading token, so a mid-message "@botname" is left
// intact, and it returns text unchanged when there is no leading command, no
// "@" in the leading token, or the "@" addresses a DIFFERENT bot. Reserved
// commands never reach this path (they are routed to their own handlers), so it
// only ever normalizes Claude pass-through commands.
func StripCommandMention(text, botUsername string) string {
	if botUsername == "" || !strings.HasPrefix(text, "/") {
		return text
	}
	// Split off only the leading token; preserve the remainder (including its
	// original whitespace) verbatim.
	rest := ""
	token := text
	if i := strings.IndexAny(text, " \t\n\r"); i >= 0 {
		token = text[:i]
		rest = text[i:]
	}
	at := strings.IndexByte(token, '@')
	if at < 0 {
		return text
	}
	mention := token[at+1:] // the addressed username, without the '@'
	if !strings.EqualFold(mention, botUsername) {
		return text // addressed to a different bot (or empty) — leave untouched
	}
	return token[:at] + rest
}

// HelpText is the static usage message replied to an allowed user who sends
// /help. It lists the slash commands the adapter understands. It is an
// engineering artifact (professional English, no duck flavor) and never reaches
// the Claude Runner.
const HelpText = "Flock Telegram assistant — available commands:\n\n" +
	"/help — show this message\n" +
	"/new — start a fresh session (forget the current conversation)\n" +
	"/stop — stop the run currently in progress\n" +
	"/schedule — manage scheduled jobs (when enabled)\n" +
	"/goal <criterion> — arm a goal an independent evaluator re-checks after every run " +
	"(/goal off to disarm)\n" +
	"/login — authorize the AI provider (Codex browser sign-in; /login status, /login cancel)\n\n" +
	"Send any other message to run it through the assistant."

// WelcomeText is the static usage message replied to an allowed user who sends
// /start. It is a short greeting prepended to the command help so a brand-new
// user immediately sees what the bot does and how to use it. Like HelpText it is
// an engineering artifact (plain English, no duck flavor) and never reaches the
// Claude Runner — the duck greeting comes from the model on a real message.
const WelcomeText = "Hi! I'm the Flock assistant.\n\n" + HelpText
