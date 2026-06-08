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

// NewSession resets the calling chat's stored Claude session so the next message
// starts a brand-new conversation with no --resume. It delegates to the session
// store's Delete, which treats an absent chat as a harmless no-op. A nil store
// (continuity disabled) is also a no-op: there is nothing to reset, and the next
// message already starts fresh. It returns any store error so the caller can log
// it; delivery of the confirmation is the caller's concern.
func (s *Service) NewSession(chatID int64) error {
	if s.sessions == nil {
		return nil
	}
	return s.sessions.Delete(chatID)
}

// StopChat cancels chatID's in-flight run, if any, via the same dispatch.Cancel
// primitive the inline Stop button uses (there is no second cancel path). It
// reports whether a run was active so the caller can tailor the acknowledgement
// ("stopping" vs "nothing to stop"). Cancel on an idle chat is a no-op.
func (s *Service) StopChat(chatID int64) bool {
	s.mu.Lock()
	running := false
	for _, c := range s.runChat {
		if c == chatID {
			running = true
			break
		}
	}
	s.mu.Unlock()
	s.dispatch.Cancel(chatID)
	return running
}
