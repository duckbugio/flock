package vk

// HelpText is the static usage message for the VK adapter. It mirrors the
// Telegram adapter's HelpText but reflects VK's command surface: VK has no native
// slash-command UI, so /stop is the universal text fallback for cancelling a run.
// It is an engineering artifact (professional English, no duck flavor).
const HelpText = "DuckFlock VK assistant — usage:\n\n" +
	"/stop — stop the run currently in progress\n\n" +
	"Send any other message to run it through the assistant."
