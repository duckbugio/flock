package telegram

import "github.com/duckbugio/flock/core/textformat"

// MarkdownToHTML renders assistant Markdown using the shared bot HTML formatter.
// Callers retain their platform-specific plain-text fallback on parse rejection.
func MarkdownToHTML(md string) string { return textformat.MarkdownToHTML(md) }
