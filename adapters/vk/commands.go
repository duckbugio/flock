package vk

import "github.com/duckbugio/flock/core/chat"

// helpTitle names the transport; everything below it is shared with the Telegram
// adapter (chat.HelpBody), so the two cannot drift apart. VK has no native
// slash-command UI, so these are plain text commands the receiver intercepts.
const helpTitle = "Flock VK assistant — available commands:\n\n"

// helpText renders the usage message. It is a thin alias over the shared render,
// so this adapter owns only its title. Unexported like welcomeText beside it:
// nothing outside this package renders VK's help.
func helpText(login chat.LoginVisibility) string { return chat.HelpText(helpTitle, login) }

// welcomeText is the reply to /start, mirroring the Telegram adapter.
func welcomeText(login chat.LoginVisibility) string { return chat.WelcomeText(helpTitle, login) }

// goalUsageText is the /goal usage reply, mirroring the Telegram adapter.
const goalUsageText = "Usage:\n" +
	"/goal <done criterion> — arm a goal; after every completed run an independent evaluator " +
	"re-checks the workspace against it and sends the team back until it holds\n" +
	"/goal — show the armed goal\n" +
	"/goal off — disarm"
