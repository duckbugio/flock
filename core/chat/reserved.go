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

// HelpBody is the shared body of both adapters' /help: the command list and the
// closing line. Only the first line — which names the transport — differs, so
// that is all an adapter supplies. It lives here, beside the canonical set it has
// to stay in step with, for the same reason as LoginHelpLine: two byte-identical
// copies share a predicate, never their words, and drift in silence.
//
// withLogin adds the /login entry, on the same condition that publishes /login in
// Telegram's command menu.
func HelpBody(login LoginVisibility) string {
	body := helpCommands
	if login == WithLogin {
		body += LoginHelpLine
	}
	return body + helpTail
}

// LoginVisibility says whether a rendered help text lists /login. It is a named
// type rather than a bare bool so a call site reads as its own documentation:
// HelpText(chat.WithLogin) instead of HelpText(true), which only means something
// after a trip to the declaration.
type LoginVisibility bool

// Whether /login belongs in a help text. Derive it from the deployment with
// LoginVisibilityFor.
const (
	WithLogin    LoginVisibility = true
	WithoutLogin LoginVisibility = false
)

// LoginVisibilityFor maps "does this deployment have an interactive sign-in" onto
// the flag, so callers do not convert a bool by hand.
func LoginVisibilityFor(applicable bool) LoginVisibility {
	if applicable {
		return WithLogin
	}
	return WithoutLogin
}

// helpCommands lists the reserved commands with the fuller wording a chat reply
// affords, next to the terse Description the command menu uses.
const helpCommands = "/help — show this message\n" +
	"/new — start a fresh session (forget the current conversation)\n" +
	"/stop — stop the run currently in progress\n" +
	"/schedule — manage scheduled jobs (when enabled)\n" +
	"/goal <criterion> — arm a goal an independent evaluator re-checks after every run " +
	"(/goal off to disarm)\n"

// helpTail closes the usage message.
const helpTail = "\nSend any other message to run it through the assistant."

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
