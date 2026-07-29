package vk

import "github.com/duckbugio/flock/core/chat"

// helpTitle names the transport; everything below it is shared with the Telegram
// adapter (chat.HelpBody), so the two cannot drift apart. VK has no native
// slash-command UI, so these are plain text commands the receiver intercepts.
const helpTitle = "Flock VK assistant — available commands:\n\n"

// HelpText renders the usage message. withLogin adds the /login line, on the same
// condition that publishes /login in Telegram's command menu. It is an
// engineering artifact (professional English, no duck flavor).
func HelpText(withLogin bool) string { return helpTitle + chat.HelpBody(withLogin) }

// welcomeText is the reply to /start: a short greeting + the usage help,
// mirroring the Telegram adapter's WelcomeText.
func welcomeText(withLogin bool) string {
	return "Hi! I'm the Flock assistant.\n\n" + HelpText(withLogin)
}

// goalUsageText is the /goal usage reply, mirroring the Telegram adapter.
const goalUsageText = "Usage:\n" +
	"/goal <done criterion> — arm a goal; after every completed run an independent evaluator " +
	"re-checks the workspace against it and sends the team back until it holds\n" +
	"/goal — show the armed goal\n" +
	"/goal off — disarm"
