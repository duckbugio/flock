package vk

import "github.com/duckbugio/flock/core/chat"

// helpHeader/helpTail bracket the command list of the VK adapter's usage
// message. It mirrors the Telegram adapter's: VK has no native slash-command UI,
// so these are plain text commands the receiver intercepts. It is an engineering
// artifact (professional English, no duck flavor).
const (
	helpHeader = "Flock VK assistant — available commands:\n\n" +
		"/help — show this message\n" +
		"/new — start a fresh session (forget the current conversation)\n" +
		"/stop — stop the run currently in progress\n" +
		"/schedule — manage scheduled jobs (when enabled)\n" +
		"/goal <criterion> — arm a goal an independent evaluator re-checks after every run " +
		"(/goal off to disarm)\n"
	helpTail = "\nSend any other message to run it through the assistant."
)

// HelpText renders the usage message. withLogin adds the /login line.
func HelpText(withLogin bool) string {
	if withLogin {
		return helpHeader + chat.LoginHelpLine + helpTail
	}
	return helpHeader + helpTail
}

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
