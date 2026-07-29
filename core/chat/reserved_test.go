package chat_test

import (
	"strings"
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
// start, help, new, stop, schedule, goal, login.
func TestReservedCommandsShape(t *testing.T) {
	wantOrder := []string{"start", "help", "new", "stop", "schedule", "goal", "login"}
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

// TestHelpBodyIsTheSharedSource: both adapters render this body, so the command
// list has one home beside the canonical set. Two byte-identical copies would
// share the applicability predicate but never the words, and would drift in
// silence — the reason LoginHelpLine moved here in the first place.
func TestHelpBodyIsTheSharedSource(t *testing.T) {
	withLogin := chat.HelpBody(chat.WithLogin)
	without := chat.HelpBody(chat.WithoutLogin)

	if !strings.Contains(withLogin, chat.LoginHelpLine) {
		t.Error("HelpBody(true) omits the /login line")
	}
	if strings.Contains(without, "/login") {
		t.Error("HelpBody(false) advertises /login where there is no sign-in")
	}
	// Every reserved command except /login is unconditional, so each must appear.
	for _, c := range chat.ReservedCommands {
		if c.Name == "login" || c.Name == "start" {
			continue // /login is conditional; /start is not listed (it IS the greeting)
		}
		if !strings.Contains(without, "/"+c.Name) {
			t.Errorf("HelpBody omits the reserved command /%s", c.Name)
		}
	}
}

// TestLoginHelpLineNamesTheDirectMessageRequirement: this line is rendered in
// group chats and published in Telegram's global command menu, so the one
// requirement that decides whether the command works at all belongs in it.
func TestLoginHelpLineNamesTheDirectMessageRequirement(t *testing.T) {
	if !strings.Contains(chat.LoginHelpLine, "direct message") {
		t.Errorf("LoginHelpLine = %q, want the direct-message requirement", chat.LoginHelpLine)
	}
	for _, c := range chat.ReservedCommands {
		if c.Name != "login" {
			continue
		}
		if !strings.Contains(c.Description, "direct message") {
			t.Errorf("the menu description %q omits the direct-message requirement", c.Description)
		}
	}
}
