package vk

// HelpText is the static usage message for the VK adapter. It mirrors the
// Telegram adapter's HelpText and lists VK's command surface: VK has no native
// slash-command UI, so these are plain text commands the receiver intercepts. It
// is an engineering artifact (professional English, no duck flavor).
const HelpText = "Flock VK assistant — available commands:\n\n" +
	"/help — show this message\n" +
	"/new — start a fresh session (forget the current conversation)\n" +
	"/stop — stop the run currently in progress\n\n" +
	"Send any other message to run it through the assistant."
