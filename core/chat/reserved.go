package chat

import "strings"

// ReservedCommand is one slash command the bot handles itself rather than
// forwarding to the selected AI provider. Name is the bare command word (no leading "/", no
// @botname suffix); Description is a concise English summary surfaced in a
// transport's command menu (e.g. Telegram's SetMyCommands).
type ReservedCommand struct {
	Name        string
	Description string
}

// ReservedCommands is the single source of truth for the slash commands the bot
// intercepts. A reserved command is handled by the bot itself and is NEVER
// forwarded to the provider Runner; every OTHER slash command (e.g. /loop,
// /security-review) is forwarded verbatim so the provider's own slash-command and
// skill system runs it. Both adapters (Telegram, VK) decide what to intercept
// from this list, and Telegram builds its command menu from it. The order here is
// the order the menu presents: start, help, new, stop, schedule, goal, login.
var ReservedCommands = []ReservedCommand{
	{Name: "start", Description: "Show a short welcome and usage"},
	{Name: "help", Description: "List the bot's own commands"},
	{Name: "new", Description: "Start a fresh session (forget the current conversation)"},
	{Name: "stop", Description: "Stop the run currently in progress"},
	{Name: "schedule", Description: "Manage scheduled jobs"},
	{Name: "goal", Description: "Arm a goal an independent evaluator re-checks after every run"},
	{Name: "login", Description: "Sign in to Codex on a subscription (other backends need no login)"},
}

// LoginHelpLine is the /login entry for an adapter's /help text. It lives here,
// beside the canonical command set, because both adapters render it and a second
// copy would drift silently — the only thing they would still share is the
// applicability predicate, not the words. Adapters append it only when an
// interactive sign-in exists, the same condition that publishes /login in
// Telegram's command menu.
const LoginHelpLine = "/login — sign in to Codex on a subscription (/login status, /login cancel)\n"

// IsReservedCommand reports whether name is one of the bot's reserved commands.
// The match is case-insensitive (name is normalized to lower case) and exact:
// only the bare command word counts, so "new" matches but "news" does not, and a
// command with a trailing space ("new ") does not match. A reserved command is
// dispatched by the bot; any other slash command is forwarded to the provider verbatim.
func IsReservedCommand(name string) bool {
	name = strings.ToLower(name)
	for _, c := range ReservedCommands {
		if c.Name == name {
			return true
		}
	}
	return false
}
