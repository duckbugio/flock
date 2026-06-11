//nolint:testpackage // whitebox: asserts the unexported botChat wiring of the rich-capability flag
package telegram

import (
	"testing"

	"github.com/go-telegram/bot"
)

// TestCapabilitiesRichFollowsFlag asserts the Bot API 10.1 rich capability is
// surfaced from the ENABLE_RICH_MESSAGES flag threaded through NewBotChat, and
// that the always-on Telegram capabilities are unaffected.
func TestCapabilitiesRichFollowsFlag(t *testing.T) {
	b, err := bot.New("123:ABC", bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	for _, enableRich := range []bool{false, true} {
		caps := NewBotChat(b, enableRich).Capabilities()
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
