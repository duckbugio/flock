//nolint:testpackage // intentionally whitebox to test unexported telegram gate internals
package chat

import (
	"bytes"
	"testing"
)

const (
	botUser       = "DuckBot"
	botUID  int64 = 4242
)

// mentionEntity builds a "mention" entity covering the leading "@username" of
// text. The offset/length are computed assuming an ASCII prefix, which the test
// strings satisfy.
func mentionEntity(offset, length int) Entity {
	return Entity{Type: EntityMention, Offset: offset, Length: length}
}

func TestShouldHandle(t *testing.T) {
	tests := []struct {
		name        string
		in          GateInput
		wantHandle  bool
		wantCleaned string
	}{
		{
			name: "DM always handled, no gate",
			in: GateInput{
				IsGroup: false, Text: "hello duck", BotUsername: botUser,
				RequireMention: true, // even with mention required, DMs bypass it
			},
			wantHandle:  true,
			wantCleaned: "hello duck",
		},
		{
			name: "group, requireMention=false: all handled",
			in: GateInput{
				IsGroup: true, Text: "just chatting", BotUsername: botUser,
				RequireMention: false,
			},
			wantHandle:  true,
			wantCleaned: "just chatting",
		},
		{
			name: "group, requireMention=true, plain chatter ignored",
			in: GateInput{
				IsGroup: true, Text: "hey folks", BotUsername: botUser,
				RequireMention: true,
			},
			wantHandle:  false,
			wantCleaned: "hey folks",
		},
		{
			name: "group, requireMention=true, @mention handled and stripped",
			in: GateInput{
				IsGroup:        true,
				Text:           "@DuckBot deploy please",
				Entities:       []Entity{mentionEntity(0, len("@DuckBot"))},
				BotUsername:    botUser,
				RequireMention: true,
			},
			wantHandle:  true,
			wantCleaned: "deploy please",
		},
		{
			name: "group, requireMention=true, mention match is case-insensitive",
			in: GateInput{
				IsGroup:        true,
				Text:           "@duckBOT status",
				Entities:       []Entity{mentionEntity(0, len("@duckBOT"))},
				BotUsername:    "DuckBot",
				RequireMention: true,
			},
			wantHandle:  true,
			wantCleaned: "status",
		},
		{
			name: "group, requireMention=true, reply-to-bot handled (no mention)",
			in: GateInput{
				IsGroup: true, Text: "yes do that", BotUsername: botUser,
				ReplyToBot: true, RequireMention: true,
			},
			wantHandle:  true,
			wantCleaned: "yes do that",
		},
		{
			name: "group, requireMention=true, @OtherBot does NOT trigger",
			in: GateInput{
				IsGroup:        true,
				Text:           "@OtherBot help",
				Entities:       []Entity{mentionEntity(0, len("@OtherBot"))},
				BotUsername:    botUser,
				RequireMention: true,
			},
			wantHandle:  false,
			wantCleaned: "@OtherBot help", // not our mention → not stripped
		},
		{
			name: "group, requireMention=true, reply to a non-bot does NOT trigger",
			in: GateInput{
				IsGroup: true, Text: "thanks", BotUsername: botUser,
				ReplyToBot: false, RequireMention: true,
			},
			wantHandle:  false,
			wantCleaned: "thanks",
		},
		{
			name: "group, requireMention=true, text_mention of the bot triggers",
			in: GateInput{
				IsGroup: true,
				Text:    "Duck do it",
				Entities: []Entity{{
					Type: EntityTextMention, Offset: 0, Length: len("Duck"), UserID: botUID,
				}},
				BotUsername:    botUser,
				BotUserID:      botUID,
				RequireMention: true,
			},
			wantHandle:  true,
			wantCleaned: "do it",
		},
		{
			name: "group, requireMention=true, text_mention of another user ignored",
			in: GateInput{
				IsGroup: true,
				Text:    "Bob do it",
				Entities: []Entity{{
					Type: EntityTextMention, Offset: 0, Length: len("Bob"), UserID: 9999,
				}},
				BotUsername:    botUser,
				BotUserID:      botUID,
				RequireMention: true,
			},
			wantHandle:  false,
			wantCleaned: "Bob do it",
		},
		{
			name: "mention not at the start is not stripped but still gates",
			in: GateInput{
				IsGroup:        true,
				Text:           "please @DuckBot deploy",
				Entities:       []Entity{mentionEntity(len("please "), len("@DuckBot"))},
				BotUsername:    botUser,
				RequireMention: true,
			},
			wantHandle:  true,
			wantCleaned: "please @DuckBot deploy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handle, cleaned := ShouldHandle(tt.in)
			if handle != tt.wantHandle {
				t.Errorf("handle = %v, want %v", handle, tt.wantHandle)
			}
			if cleaned != tt.wantCleaned {
				t.Errorf("cleaned = %q, want %q", cleaned, tt.wantCleaned)
			}
		})
	}
}

// TestStripLeadingMentionUTF16 verifies entity offsets are interpreted as UTF-16
// code units (Telegram's convention) in the presence of astral-plane characters,
// covering both the non-stripping (mid-string mention) and stripping (leading
// mention with an emoji in the remainder) paths. A byte- or rune-based slice
// would mis-align and either mis-match the mention or garble the remainder.
func TestStripLeadingMentionUTF16(t *testing.T) {
	t.Run("mid-string mention detected but not stripped", func(t *testing.T) {
		// "🦆 @DuckBot go": the duck emoji is one astral-plane rune = 2 UTF-16
		// units, the following space is 1, so "@DuckBot" starts at unit offset 3.
		text := "🦆 @DuckBot go"
		ents := []Entity{{Type: EntityMention, Offset: 3, Length: len("@DuckBot")}}
		handle, cleaned := ShouldHandle(GateInput{
			IsGroup: true, Text: text, Entities: ents,
			BotUsername: botUser, RequireMention: true,
		})
		if !handle {
			t.Fatal("expected the bot mention to be detected")
		}
		// The mention sits mid-string (offset != 0), so it is not stripped; the
		// gate still fires. This asserts UTF-16 slicing picks the right substring
		// for the match rather than a byte-misaligned one.
		if cleaned != text {
			t.Errorf("cleaned = %q, want unchanged %q", cleaned, text)
		}
	})

	t.Run("leading mention stripped, astral remainder byte-correct", func(t *testing.T) {
		// "@DuckBot 🦆 deploy 🚀": a leading bot mention (offset 0) followed by a
		// space and a remainder that contains astral-plane emoji. The mention must
		// be stripped and the emoji-bearing remainder preserved byte-for-byte.
		text := "@DuckBot 🦆 deploy 🚀"
		ents := []Entity{{Type: EntityMention, Offset: 0, Length: len("@DuckBot")}}
		handle, cleaned := ShouldHandle(GateInput{
			IsGroup: true, Text: text, Entities: ents,
			BotUsername: botUser, RequireMention: true,
		})
		if !handle {
			t.Fatal("expected the leading bot mention to be detected")
		}
		const want = "🦆 deploy 🚀"
		if cleaned != want {
			t.Errorf("cleaned = %q, want %q", cleaned, want)
		}
		// Assert the exact bytes too, to catch any silent UTF-16/byte mis-slicing
		// that garbles the astral-plane runes rather than reporting a clean mismatch.
		if !bytes.Equal([]byte(cleaned), []byte(want)) {
			t.Errorf("cleaned bytes = % x, want % x", []byte(cleaned), []byte(want))
		}
	})
}
