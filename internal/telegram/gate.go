package telegram

import (
	"strings"
)

// EntityType identifies the kinds of message entities the mention-gate cares
// about. Only "mention" (a plain @username) and "text_mention" (a mention of a
// user without a username, carrying the User object) are relevant; everything
// else is ignored. These string values match Telegram's Bot API entity types.
type EntityType string

const (
	// EntityMention is a plain "@username" mention.
	EntityMention EntityType = "mention"
	// EntityTextMention is a mention of a user that has no public username; the
	// entity carries the mentioned user (so it is matched by id, not by text).
	EntityTextMention EntityType = "text_mention"
)

// Entity is the transport-agnostic subset of a Telegram message entity the gate
// needs: its type, where it sits in the text, and — for a text_mention — the id
// of the mentioned user. The handler maps models.MessageEntity onto this so the
// decision logic stays testable without the Telegram library.
type Entity struct {
	Type   EntityType
	Offset int   // start, in UTF-16 code units, per the Telegram Bot API
	Length int   // length, in UTF-16 code units
	UserID int64 // mentioned user's id, for text_mention; 0 otherwise
}

// GateInput is the minimal set of fields the mention-gate decision needs. It is
// deliberately free of any Telegram type so the decision is a pure function the
// unit tests can drive directly (AC1).
type GateInput struct {
	// IsGroup is true for group/supergroup chats and false for private chats
	// (DMs). DMs are never gated.
	IsGroup bool
	// Text is the message text (or caption); the leading @mention of the bot is
	// stripped from the returned cleaned text.
	Text string
	// Entities are the message's text/caption entities (already concatenated by
	// the handler), used to locate an @mention of the bot.
	Entities []Entity
	// ReplyToBot is true when the message is a reply to one of the bot's own
	// messages.
	ReplyToBot bool
	// BotUsername is the bot's @username (with or without the leading "@");
	// matched case-insensitively against "mention" entities.
	BotUsername string
	// BotUserID is the bot's Telegram user id; matched against "text_mention"
	// entities (mentions of users without a username).
	BotUserID int64
	// RequireMention reflects REQUIRE_GROUP_MENTION: when false the bot acts on
	// every group message; when true a group message is only handled if it
	// @mentions the bot or replies to the bot.
	RequireMention bool
}

// ShouldHandle decides whether a message should be processed and returns the
// text to send to Claude with any leading @mention of the bot stripped.
//
// Mention-gating rules:
//   - DMs (private chats) are always handled, unchanged.
//   - Groups with RequireMention=false are always handled (the default).
//   - Groups with RequireMention=true are handled only when the message either
//     @mentions the bot (a "mention" entity matching BotUsername, or a
//     "text_mention" entity matching BotUserID) or replies to the bot.
//
// On a handled message the bot's @mention is stripped from the text when it
// appears at the start (so Claude does not see "@bot ..."). The cleaned text is
// always returned, even when handle is false, so callers may ignore it safely.
func ShouldHandle(in GateInput) (handle bool, cleaned string) {
	cleaned = stripLeadingMention(in.Text, in.Entities, in.BotUsername, in.BotUserID)

	// Private chats and groups that don't require a mention: always handled.
	if !in.IsGroup || !in.RequireMention {
		return true, cleaned
	}

	if in.ReplyToBot {
		return true, cleaned
	}
	if mentionsBot(in.Text, in.Entities, in.BotUsername, in.BotUserID) {
		return true, cleaned
	}
	return false, cleaned
}

// normalizeUsername lower-cases a username and trims a single leading "@".
func normalizeUsername(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// mentionsBot reports whether any entity is a mention of this bot: a "mention"
// entity whose text equals BotUsername, or a "text_mention" entity whose user
// is the bot. The comparison is case-insensitive. A non-bot @mention (e.g.
// @OtherBot) or a text_mention of a different user does not match.
func mentionsBot(text string, entities []Entity, botUsername string, botUserID int64) bool {
	uname := normalizeUsername(botUsername)
	for _, ent := range entities {
		switch ent.Type {
		case EntityMention:
			if uname == "" {
				continue
			}
			if normalizeUsername(entitySlice(text, ent)) == uname {
				return true
			}
		case EntityTextMention:
			if botUserID != 0 && ent.UserID == botUserID {
				return true
			}
		}
	}
	return false
}

// stripLeadingMention removes a leading @mention of the bot (plus the whitespace
// that follows it) from text, when the message begins with that mention. A
// mention elsewhere in the text is left intact, matching how a human would
// address the bot ("@bot do X" → "do X"). Mirrors the Python adapter, which
// gates on the mention but passes the user's words to Claude.
func stripLeadingMention(text string, entities []Entity, botUsername string, botUserID int64) string {
	uname := normalizeUsername(botUsername)
	for _, ent := range entities {
		if ent.Offset != 0 {
			continue // only strip a mention at the very start of the message
		}
		isBotMention := false
		switch ent.Type {
		case EntityMention:
			if uname != "" && normalizeUsername(entitySlice(text, ent)) == uname {
				isBotMention = true
			}
		case EntityTextMention:
			if botUserID != 0 && ent.UserID == botUserID {
				isBotMention = true
			}
		}
		if !isBotMention {
			continue
		}
		// Cut the mention off the front, then trim the whitespace it left behind.
		rest := utf16Slice(text, ent.Offset+ent.Length, -1)
		return strings.TrimLeft(rest, " \t\n")
	}
	return text
}

// entitySlice returns the substring of text covered by an entity. Telegram
// entity offsets/lengths are in UTF-16 code units; utf16Slice converts.
func entitySlice(text string, ent Entity) string {
	return utf16Slice(text, ent.Offset, ent.Offset+ent.Length)
}

// utf16Slice slices text by UTF-16 code-unit indices, as Telegram specifies for
// entity offset/length. end == -1 means "to the end of the string". This keeps
// mention extraction correct in the presence of emoji and other astral-plane
// characters.
func utf16Slice(text string, start, end int) string {
	units := 0
	var startByte, endByte = -1, len(text)
	if start == 0 {
		startByte = 0
	}
	for i, r := range text {
		if units == start && startByte == -1 {
			startByte = i
		}
		if end >= 0 && units == end {
			endByte = i
			break
		}
		if r > 0xFFFF {
			units += 2 // surrogate pair
		} else {
			units++
		}
	}
	if startByte == -1 {
		startByte = len(text)
	}
	if startByte > endByte {
		startByte = endByte
	}
	return text[startByte:endByte]
}
