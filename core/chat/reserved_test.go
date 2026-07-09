package chat_test

import (
	"testing"

	"github.com/duckbugio/flock/core/chat"
)

// TestIsReservedCommand asserts the canonical reserved set: the four bot-handled
// commands match case-insensitively, while every other slash command (a Claude
// pass-through), the empty string, a near-miss like "news", and a name with a
// stray trailing space do NOT match — that exactness is what keeps /news and
// /loop forwarded to Claude.
func TestIsReservedCommand(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"start", true},
		{"help", true},
		{"new", true},
		{"stop", true},
		{"schedule", true},
		{"START", true},
		{"Help", true},
		{"NeW", true},
		{"STOP", true},
		{"Schedule", true},
		{"goal", true},
		{"GOAL", true},
		{"loop", false},
		{"news", false},
		{"", false},
		{"new ", false},
		{" new", false},
		{"security-review", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chat.IsReservedCommand(tc.name); got != tc.want {
				t.Fatalf("IsReservedCommand(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestReservedCommandsShape guards the menu source: names are unique, lowercase,
// non-empty, every command has a description, and the order is the documented
// start, help, new, stop, schedule, goal.
func TestReservedCommandsShape(t *testing.T) {
	wantOrder := []string{"start", "help", "new", "stop", "schedule", "goal"}
	if len(chat.ReservedCommands) != len(wantOrder) {
		t.Fatalf("ReservedCommands has %d entries, want %d", len(chat.ReservedCommands), len(wantOrder))
	}

	seen := make(map[string]bool, len(chat.ReservedCommands))
	for i, c := range chat.ReservedCommands {
		if c.Name != wantOrder[i] {
			t.Errorf("ReservedCommands[%d].Name = %q, want %q", i, c.Name, wantOrder[i])
		}
		if c.Name != lower(c.Name) {
			t.Errorf("ReservedCommands[%d].Name = %q is not lowercase", i, c.Name)
		}
		if c.Name == "" {
			t.Errorf("ReservedCommands[%d] has an empty Name", i)
		}
		if c.Description == "" {
			t.Errorf("ReservedCommands[%d] (%q) has an empty Description", i, c.Name)
		}
		if seen[c.Name] {
			t.Errorf("ReservedCommands has a duplicate Name %q", c.Name)
		}
		seen[c.Name] = true
	}

	if !chat.IsReservedCommand("schedule") {
		t.Error("schedule must be reserved (the bot owns /schedule)")
	}
}

// lower is a tiny local helper so the shape test does not import strings just to
// assert lowercase-ness.
func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + ('a' - 'A')
		}
	}
	return string(out)
}
