package vk

import (
	"regexp"
	"strconv"
	"strings"
)

// mentionPattern matches a VK community mention token: [club<id>|display] or
// [public<id>|display]. VK renders a mention of a community in message text this
// way (the leading char is "*" or "@" in some clients, but the canonical bot-API
// form is the bracketed token). The id group is captured so we only treat a
// mention of THIS community as addressing the bot.
var mentionPattern = regexp.MustCompile(`\[(?:club|public)(\d+)\|[^\]]*\]`)

// parseMention reports whether text mentions the community groupID and returns
// the text with that mention token stripped (and surrounding whitespace
// trimmed). A mention of a DIFFERENT community is left intact and does not count
// as addressing the bot. When groupID's mention appears, every occurrence of it
// is removed and the result is space-collapsed at the edges so Claude sees the
// user's words without the "[club123|Bot]" noise — mirroring how the Telegram
// adapter strips a leading @mention before handing text to the gate.
func parseMention(text string, groupID int64) (mentioned bool, stripped string) {
	want := strconv.FormatInt(groupID, 10)
	stripped = mentionPattern.ReplaceAllStringFunc(text, func(tok string) string {
		m := mentionPattern.FindStringSubmatch(tok)
		if len(m) == 2 && m[1] == want {
			mentioned = true
			return ""
		}
		return tok // a different community's mention stays in place
	})
	if mentioned {
		stripped = strings.TrimSpace(stripped)
	}
	return mentioned, stripped
}

// shouldHandle applies the transport-agnostic mention-gate decision to a VK group
// message, after parsing VK's own [club<id>|...] mention. It mirrors the Telegram
// adapter's handleMessage shape: the adapter parses the platform mention, then
// the neutral chat.ShouldHandle makes the decision. Because VK mentions are
// textual (not entity-spanned like Telegram's), the parse + strip happens here
// and the result is fed to the gate as a pre-resolved "addressed + cleaned text".
//
// Rules (identical to the neutral gate): DMs and groups with requireMention=false
// are always handled; a group with requireMention=true is handled only when the
// message mentions this community. The returned cleaned text always has this
// community's mention stripped, even when handle is false.
func shouldHandle(isGroup bool, text string, groupID int64, requireMention bool) (handle bool, cleaned string) {
	mentioned, cleaned := parseMention(text, groupID)

	// The decision mirrors core/chat.ShouldHandle exactly (DMs and non-require
	// groups always handled; a require-mention group needs a mention), but the
	// addressed/stripped inputs are pre-resolved here because VK mentions are
	// textual [club<id>|...] tokens, not the entity spans the neutral gate parses
	// for Telegram — so this is the VK half of the §4 mention-gate split.
	if !isGroup || !requireMention {
		return true, cleaned
	}
	return mentioned, cleaned
}
