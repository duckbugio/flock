package chat

import (
	"context"
	"io"
)

// ChatID is an opaque, transport-defined chat identifier. Telegram uses numeric
// chat ids; VK peer ids are numeric too but other platforms' ids may be strings,
// so it is widened to string and each adapter converts its native id to/from
// string at its own boundary. It is a transparent alias (not a distinct named
// type) so the id flows through the conversation layer without explicit casts.
// The deliberately descriptive name (over the linter's "ID") keeps the
// Transport contract readable alongside MessageID.
//
//nolint:revive // name intentionally matches the multi-transport contract (see docs/multi-transport-plan.md §4).
type ChatID = string

// MessageID is an opaque, transport-defined message identifier, widened to
// string for the same reason as ChatID (Telegram's int message id is rendered
// to a string at the adapter boundary).
type MessageID = string

// Transport abstracts the chat operations the run loop needs, so the loop can be
// driven by any platform (and by a fake in tests). It is the generalized form of
// the original internal/telegram.chat interface: every method and parameter is
// preserved (including asMarkdown), with the chat/message ids widened to the
// ChatID/MessageID string aliases.
type Transport interface {
	// Send posts a new message with optional Stop markup and returns its id. When
	// asMarkdown is true the text is rendered as the platform's rich format (with a
	// plain-text fallback on a parse error); false sends it verbatim as plain text.
	Send(ctx context.Context, chatID ChatID, text, stopRunID string, asMarkdown bool) (messageID MessageID, err error)
	// SendReply posts a new message as a native reply to replyToID (so tapping it
	// jumps to that message), carrying no Stop markup, and returns its id. It is a
	// sibling of Send rather than an extra Send parameter so existing Send call
	// sites and fakes stay untouched. asMarkdown behaves as for Send. If replyToID
	// no longer exists the platform must still deliver the message without the reply
	// (Telegram: allow_sending_without_reply); a platform that cannot reply to the
	// id at all falls back to a plain send. Used for the completion notice, which
	// replies to the run's own answer/anchor message.
	SendReply(
		ctx context.Context, chatID ChatID, replyToID MessageID, text string, asMarkdown bool,
	) (messageID MessageID, err error)
	// Edit updates an existing message's text (and Stop markup, when stopRunID is
	// non-empty; empty clears the markup). asMarkdown behaves as for Send.
	Edit(ctx context.Context, chatID ChatID, messageID MessageID, text, stopRunID string, asMarkdown bool) error
	// Delete removes a message; failures are non-fatal to the caller.
	Delete(ctx context.Context, chatID ChatID, messageID MessageID) error
	// SendDocument uploads a local file to the chat as a document/attachment. The
	// data is streamed (no token-bearing URL is constructed). Used by the outbox.
	SendDocument(ctx context.Context, chatID ChatID, name string, data io.Reader) error
	// SendStarNudge posts the post-task star-nudge message carrying the inline
	// confirm button (a callback button, so pressing it triggers the server-side
	// star). It is a separate seam from Send because the nudge needs the star
	// markup, not the Stop markup. Returns the new message id.
	SendStarNudge(ctx context.Context, chatID ChatID, text string) (messageID MessageID, err error)
	// Capabilities reports what this platform can do, so the Service can adapt to
	// platforms weaker than Telegram instead of erroring on a missing primitive.
	Capabilities() Capabilities
}

// Capabilities lets the Service adapt to platforms weaker than Telegram, with a
// defined fallback so a missing primitive never breaks delivery. Telegram and VK
// set CanSendDocument true and MaxMessageRunes 4096.
type Capabilities struct {
	// CanSendDocument: platform supports file attachments. false → the outbox
	// sweep is skipped. (Telegram/VK: true.)
	CanSendDocument bool
	// MaxMessageRunes is the platform's max message length in runes, fed to the
	// chunker (Telegram 4096). A non-positive value selects a safe default.
	MaxMessageRunes int
	// CanSendRich: the platform can render Bot API 10.1 rich messages (structured
	// blocks) instead of the legacy MarkdownToHTML/plain path. It is purely additive
	// — false (the zero value) keeps a transport on the exact pre-rich behaviour, so
	// a platform that does not set it (VK) needs no change. Telegram sets it from the
	// ENABLE_RICH_MESSAGES flag; even there the
	// rich path always falls back to MarkdownToHTML/plain on any error, so the flag
	// is a feature toggle, never a hard dependency.
	CanSendRich bool
}
