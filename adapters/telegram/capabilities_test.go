//nolint:testpackage // whitebox: asserts the unexported botChat wiring of the rich-capability flag
package telegram

import "testing"

// TestCapabilitiesRichFollowsFlag asserts the Bot API 10.1 rich capability is
// surfaced from the ENABLE_RICH_MESSAGES flag threaded through NewBotChat, and
// that the always-on Telegram capabilities are unaffected. Capabilities() does
// not touch the *bot.Bot, so a nil bot is sufficient here.
func TestCapabilitiesRichFollowsFlag(t *testing.T) {
	for _, enableRich := range []bool{false, true} {
		caps := NewBotChat(nil, enableRich).Capabilities()
		if caps.CanSendRich != enableRich {
			t.Errorf("NewBotChat(_, %t).Capabilities().CanSendRich = %t, want %t",
				enableRich, caps.CanSendRich, enableRich)
		}
		if !caps.CanSendDocument {
			t.Errorf("CanSendDocument = false, want true (enableRich=%t)", enableRich)
		}
		if caps.MaxMessageRunes != telegramMaxMessageRunes {
			t.Errorf("MaxMessageRunes = %d, want %d", caps.MaxMessageRunes, telegramMaxMessageRunes)
		}
	}
}
