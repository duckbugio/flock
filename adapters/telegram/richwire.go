package telegram

import "github.com/duckbugio/flock/core/chat/rich"

// ============================================================================
// PROVISIONAL Bot API 10.1 rich-message wire types.
//
// This is the ONE place that encodes the on-the-wire JSON shape of the rich
// message payload. As of this writing the schema is NOT yet verifiable: the
// upstream go-telegram/bot lib (v1.21.0, the latest) does not model these types,
// and the published Bot API 10.1 changelog lists the class NAMES
// (InputRichMessage, RichBlockParagraph, RichBlockPreformatted, RichBlockTable,
// …) but not their field-level JSON. The field names and the "type" discriminator
// strings below are therefore derived from those published class names plus
// Telegram's invariant JSON conventions (snake_case fields; a "type" tag on
// polymorphic objects, as InputMedia/PassportElementError use).
//
// This is safe because the whole rich path is OFF by default (ENABLE_RICH_MESSAGES)
// and every call site falls back to the legacy MarkdownToHTML/plain rendering on
// ANY error — so a wrong field name degrades to today's output, never a failure.
// When the lib gains the types (swap to them) or a live API/sample verifies the
// schema, THIS FILE is the only thing to correct; the serializer and transport
// above/below it stay put.
// ============================================================================

// Block "type" discriminator strings (snake_cased from the RichBlock* class
// names). Only the blocks the serializer currently emits are defined.
const (
	blockParagraph    = "paragraph"       // RichBlockParagraph
	blockHeading      = "section_heading" // RichBlockSectionHeading
	blockPreformatted = "preformatted"    // RichBlockPreformatted
	blockList         = "list"            // RichBlockList
	blockQuote        = "block_quotation" // RichBlockBlockQuotation
	blockTable        = "table"           // RichBlockTable
)

// inputRichMessage is the top-level payload of sendRichMessage /
// editMessageText(rich_message) / sendRichMessageDraft.
type inputRichMessage struct {
	Blocks []richBlock `json:"blocks"`
}

// richBlock is one polymorphic block, discriminated by Type. A single flat struct
// (rather than an interface with a custom marshaler) keeps the provisional wire
// definition in one readable place; unused fields are omitted via omitempty.
type richBlock struct {
	Type     string     `json:"type"`
	Text     *richText  `json:"text,omitempty"`     // paragraph, section_heading, block_quotation
	Level    int        `json:"level,omitempty"`    // section_heading (1..6)
	Language string     `json:"language,omitempty"` // preformatted (fence info-string)
	Code     string     `json:"code,omitempty"`     // preformatted (verbatim body)
	Ordered  bool       `json:"ordered,omitempty"`  // list
	Items    []richText `json:"items,omitempty"`    // list
	Header   []string   `json:"header,omitempty"`   // table
	Rows     [][]string `json:"rows,omitempty"`     // table
}

// richText is a run of formatted inline text. The published RichText* styling
// classes (RichTextBold, RichTextCode, RichTextUrl, …) imply a per-run style; we
// model the subset FromMarkdown emits (bold, inline code, links) as flags on each
// run.
type richText struct {
	Runs []richTextRun `json:"runs"`
}

type richTextRun struct {
	Text string `json:"text"`
	Bold bool   `json:"bold,omitempty"`
	Code bool   `json:"code,omitempty"`
	URL  string `json:"url,omitempty"` // non-empty → the run is a link
}

// ----------------------------------------------------------------------------
// Serializer: neutral rich IR -> the wire payload. This mapping is stable and
// fully testable regardless of the provisional field names above.
// ----------------------------------------------------------------------------

// toInputRichMessage converts the neutral rich.Message IR into the wire payload.
// It is total: an unmapped block type degrades to an empty paragraph so the
// payload is always valid (mirroring FromMarkdown's best-effort guarantee).
func toInputRichMessage(m rich.Message) inputRichMessage {
	out := inputRichMessage{}
	for _, b := range m.Blocks {
		out.Blocks = append(out.Blocks, toRichBlock(b))
	}
	return out
}

func toRichBlock(b rich.Block) richBlock {
	switch v := b.(type) {
	case rich.Paragraph:
		return richBlock{Type: blockParagraph, Text: toRichText(v.Text)}
	case rich.Heading:
		return richBlock{Type: blockHeading, Level: v.Level, Text: toRichText(v.Text)}
	case rich.Code:
		return richBlock{Type: blockPreformatted, Language: v.Lang, Code: v.Text}
	case rich.List:
		return richBlock{Type: blockList, Ordered: v.Ordered, Items: toRichTexts(v.Items)}
	case rich.Quote:
		return richBlock{Type: blockQuote, Text: toRichText(v.Text)}
	case rich.Table:
		return richBlock{Type: blockTable, Header: v.Header, Rows: v.Rows}
	default:
		return richBlock{Type: blockParagraph, Text: &richText{}}
	}
}

func toRichText(in rich.Inline) *richText {
	rt := &richText{}
	for _, s := range in.Spans {
		rt.Runs = append(rt.Runs, richTextRun{Text: s.Text, Bold: s.Bold, Code: s.Code, URL: s.URL})
	}
	return rt
}

func toRichTexts(in []rich.Inline) []richText {
	out := make([]richText, 0, len(in))
	for _, t := range in {
		out = append(out, *toRichText(t))
	}
	return out
}
