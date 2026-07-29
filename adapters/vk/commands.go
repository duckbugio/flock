package vk

// HelpText is the static usage message for the VK adapter. It mirrors the
// Telegram adapter's HelpText and lists VK's command surface: VK has no native
// slash-command UI, so these are plain text commands the receiver intercepts. It
// is an engineering artifact (professional English, no duck flavor).
const HelpText = "Flock VK assistant — available commands:\n\n" +
	"/help — show this message\n" +
	"/new — start a fresh session (forget the current conversation)\n" +
	"/stop — stop the run currently in progress\n" +
	"/schedule — manage scheduled jobs (when enabled)\n" +
	"/goal <criterion> — arm a goal an independent evaluator re-checks after every run " +
	"(/goal off to disarm)\n" +
	"/login — authorize the AI provider (Codex browser sign-in; /login status, /login cancel)\n\n" +
	"Send any other message to run it through the assistant."

// goalUsageText is the /goal usage reply, mirroring the Telegram adapter.
const goalUsageText = "Usage:\n" +
	"/goal <done criterion> — arm a goal; after every completed run an independent evaluator " +
	"re-checks the workspace against it and sends the team back until it holds\n" +
	"/goal — show the armed goal\n" +
	"/goal off — disarm"
