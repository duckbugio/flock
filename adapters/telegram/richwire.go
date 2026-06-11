package telegram

// Bot API 10.1 rich-message wire types, VERIFIED against the official reference
// (core.telegram.org/bots/api — InputRichMessage / sendRichMessage / the "Rich
// Markdown style" section).
//
// Sending a rich message does NOT use the RichBlock*/RichText* tree — those
// describe a RECEIVED RichMessage. To SEND, InputRichMessage carries exactly one
// of `markdown` or `html`: a single string in Telegram's "Rich Markdown style"
// (an extended GitHub-flavoured Markdown — headings, tables, task lists,
// blockquotes, ~~strikethrough~~, ==marked==, ||spoiler||, <details>, footnotes,
// $math$, …). The assistant already emits this dialect, so the rich path passes
// its Markdown straight through and lets Telegram parse it — no IR, no serializer.
type inputRichMessage struct {
	// Markdown is the message content in Telegram's Rich Markdown style. Exactly one
	// of Markdown or HTML must be set; the adapter always uses Markdown.
	Markdown string `json:"markdown,omitempty"`
	// HTML is the alternative content form (unused by this adapter, kept for
	// completeness of the documented type).
	HTML string `json:"html,omitempty"`
	// IsRTL requests right-to-left rendering. Left false (default LTR).
	IsRTL bool `json:"is_rtl,omitempty"` //nolint:tagliatelle // Telegram Bot API uses snake_case.
	// SkipEntityDetection disables Telegram's automatic detection of URLs, e-mails,
	// @mentions, #hashtags, $cashtags, phone numbers, /commands and bank-card-like
	// digit runs in the text. We set it true for the rich path: the legacy
	// MarkdownToHTML path only ever linked EXPLICIT [text](url) markdown, so leaving
	// auto-detection on would silently turn ordinary dev output (identifiers like
	// @scope/pkg, issue refs #4242, $ENV vars, long numeric ids) into surprise
	// entities — a behaviour change, not parity. Explicit Markdown links still work.
	SkipEntityDetection bool `json:"skip_entity_detection,omitempty"` //nolint:tagliatelle // Telegram Bot API uses snake_case.
}

// richMarkdown builds an InputRichMessage from the assistant's Markdown text. It
// disables automatic entity detection so the rich path matches the legacy path's
// "explicit links only" behaviour (see SkipEntityDetection).
func richMarkdown(text string) inputRichMessage {
	return inputRichMessage{Markdown: text, SkipEntityDetection: true}
}
