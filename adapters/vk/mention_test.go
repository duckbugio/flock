//nolint:testpackage // intentionally whitebox to test unexported VK mention/gate internals
package vk

import "testing"

func TestParseMentionStripsThisCommunity(t *testing.T) {
	const groupID = 123
	for _, tc := range []struct {
		name          string
		text          string
		wantMentioned bool
		wantStripped  string
	}{
		{"club mention leading", "[club123|Duck Bot] please help", true, "please help"},
		{"public mention", "[public123|Duck] do X", true, "do X"},
		{"mention in middle", "hey [club123|Duck] now", true, "hey  now"},
		{"other community", "[club999|Other] hi", false, "[club999|Other] hi"},
		{"no mention", "just a message", false, "just a message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mentioned, stripped := parseMention(tc.text, groupID)
			if mentioned != tc.wantMentioned {
				t.Errorf("mentioned = %v, want %v", mentioned, tc.wantMentioned)
			}
			if stripped != tc.wantStripped {
				t.Errorf("stripped = %q, want %q", stripped, tc.wantStripped)
			}
		})
	}
}

func TestShouldHandleGate(t *testing.T) {
	const groupID = 123
	for _, tc := range []struct {
		name           string
		isGroup        bool
		text           string
		requireMention bool
		replyToBot     bool
		wantHandle     bool
		wantCleaned    string
	}{
		{"dm always handled", false, "hello", true, false, true, "hello"},
		{"group no-require always handled", true, "hello", false, false, true, "hello"},
		{"group require + mention", true, "[club123|Duck] hi", true, false, true, "hi"},
		{"group require + no mention ignored", true, "hello", true, false, false, "hello"},
		{"group require + reply handled", true, "hello", true, true, true, "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle, cleaned := shouldHandle(tc.isGroup, tc.text, groupID, tc.requireMention, tc.replyToBot)
			if handle != tc.wantHandle {
				t.Errorf("handle = %v, want %v", handle, tc.wantHandle)
			}
			if cleaned != tc.wantCleaned {
				t.Errorf("cleaned = %q, want %q", cleaned, tc.wantCleaned)
			}
		})
	}
}
