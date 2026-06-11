// Package rich is a platform-neutral intermediate representation (IR) for
// structured "rich" messages, plus a Markdown parser (FromMarkdown) that builds
// it. It is the shared core the Telegram adapter will serialize to Bot API 10.1
// rich messages (see docs/rich-messages-plan.md). It has no platform imports and
// is fully unit-testable in isolation; other adapters (VK) never import it, so
// adding this package cannot change their behaviour.
//
// Scope (Stage 1): the IR models exactly what FromMarkdown produces today — the
// same Markdown subset MarkdownToHTML recognizes (fenced/inline code, **/__ bold,
// ATX headings, [text](url) links, -/*/+ bullets, blockquotes), plus the
// unambiguous extras the rich target can render that flat HTML could not (ordered
// lists and pipe tables). Blocks that have no Markdown source and are built
// programmatically by later stages (a Thinking reasoning block, a collapsible
// Details block) are intentionally NOT defined here — each is added with the
// stage that produces it, so the IR never carries an unconstructed type.
package rich

// Span is one run of inline text carrying zero or more independent style flags.
// The model is deliberately FLAT (no nesting) and best-effort, matching
// MarkdownToHTML's conservatism. Code and URL are special: a Code span's Text is
// the verbatim code (never re-parsed), and a span with a non-empty URL is a link
// to that target. Only the styles FromMarkdown emits are modelled (Bold, Code,
// links); italic/strikethrough are omitted on purpose — single-`*`/`_` emphasis
// false-matches the snake_case and glob text developers send constantly, so it is
// not parsed, and a field with no producer would be dead weight.
type Span struct {
	// Text is the span's literal text (already unescaped; for a Code span it is the
	// verbatim code between the backticks).
	Text string
	// URL, when non-empty, makes this span a link to that target. Text is the link
	// label.
	URL string
	// Bold marks the span as strong/bold (Markdown **…** or __…__).
	Bold bool
	// Code marks the span as inline code (Markdown `…`); Text is verbatim.
	Code bool
}

// Inline is a sequence of styled spans forming one logical run of text (one
// heading, one paragraph, one list item, one blockquote).
type Inline struct {
	Spans []Span
}

// Block is one structural element of a rich Message. The set is a small sealed
// interface (the isBlock marker is unexported) so a serializer can switch over it
// exhaustively without a default surprise.
type Block interface{ isBlock() }

// Paragraph is a run of body text.
type Paragraph struct{ Text Inline }

// Heading is a section heading; Level is 1..6 (ATX #..######).
type Heading struct {
	Level int
	Text  Inline
}

// Code is a preformatted code block; Lang is the optional fence info-string
// (e.g. "go"), Text is the verbatim body with no trailing newline.
type Code struct {
	Lang string
	Text string
}

// List is a bullet (Ordered=false) or numbered (Ordered=true) list. Each item is
// a single inline run; nested lists are flattened best-effort into items.
type List struct {
	Ordered bool
	Items   []Inline
}

// Quote is a blockquote; consecutive ">" lines are joined into one inline run.
type Quote struct{ Text Inline }

// Table is a pipe table: a header row plus zero or more body rows, each a slice
// of already-trimmed cell strings. Cells are plain text (not re-parsed inline) in
// Stage 1 — kept simple and unambiguous.
type Table struct {
	Header []string
	Rows   [][]string
}

func (Paragraph) isBlock() {}
func (Heading) isBlock()   {}
func (Code) isBlock()      {}
func (List) isBlock()      {}
func (Quote) isBlock()     {}
func (Table) isBlock()     {}

// Message is a complete rich message: an ordered list of blocks.
type Message struct {
	Blocks []Block
}
