//nolint:testpackage // whitebox: tests the unexported rich wire serializer
package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/chat/rich"
)

func span(text string) rich.Inline { return rich.Inline{Spans: []rich.Span{{Text: text}}} }

// TestToInputRichMessageMapsEveryBlock asserts each neutral IR block maps to the
// expected wire block type and carries its fields.
func TestToInputRichMessageMapsEveryBlock(t *testing.T) {
	m := rich.Message{Blocks: []rich.Block{
		rich.Heading{Level: 2, Text: span("Title")},
		rich.Paragraph{Text: rich.Inline{Spans: []rich.Span{
			{Text: "see "},
			{Text: "docs", URL: "https://x.y"},
			{Text: " and "},
			{Text: "b", Bold: true},
			{Text: "c", Code: true},
		}}},
		rich.Code{Lang: "go", Text: "x := 1"},
		rich.List{Ordered: true, Items: []rich.Inline{span("one"), span("two")}},
		rich.Quote{Text: span("q")},
		rich.Table{Header: []string{"H1", "H2"}, Rows: [][]string{{"a", "b"}}},
	}}

	got := toInputRichMessage(m)
	if len(got.Blocks) != len(m.Blocks) {
		t.Fatalf("block count = %d, want %d", len(got.Blocks), len(m.Blocks))
	}

	if h := got.Blocks[0]; h.Type != blockHeading || h.Level != 2 || h.Text == nil || h.Text.Runs[0].Text != "Title" {
		t.Errorf("heading block = %+v", h)
	}
	if p := got.Blocks[1]; p.Type != blockParagraph || p.Text == nil || len(p.Text.Runs) != 5 {
		t.Fatalf("paragraph block = %+v", p)
	} else {
		runs := p.Text.Runs
		if runs[1].URL != "https://x.y" || runs[1].Text != "docs" {
			t.Errorf("link run = %+v", runs[1])
		}
		if !runs[3].Bold || !runs[4].Code {
			t.Errorf("bold/code runs = %+v / %+v", runs[3], runs[4])
		}
	}
	if c := got.Blocks[2]; c.Type != blockPreformatted || c.Language != "go" || c.Code != "x := 1" {
		t.Errorf("code block = %+v", c)
	}
	if l := got.Blocks[3]; l.Type != blockList || !l.Ordered || len(l.Items) != 2 || l.Items[0].Runs[0].Text != "one" {
		t.Errorf("list block = %+v", l)
	}
	if q := got.Blocks[4]; q.Type != blockQuote || q.Text == nil || q.Text.Runs[0].Text != "q" {
		t.Errorf("quote block = %+v", q)
	}
	if tb := got.Blocks[5]; tb.Type != blockTable || len(tb.Header) != 2 || tb.Rows[0][1] != "b" {
		t.Errorf("table block = %+v", tb)
	}
}

// TestToInputRichMessageJSONShape asserts the serialized payload carries the
// "blocks"/"type"/"runs" shape the wire expects (a regression guard on the
// provisional schema — the single place to update if field names are corrected).
func TestToInputRichMessageJSONShape(t *testing.T) {
	m := rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: span("hi")}}}
	b, err := json.Marshal(toInputRichMessage(m))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, want := range []string{`"blocks"`, `"type":"paragraph"`, `"runs"`, `"text":"hi"`} {
		if !strings.Contains(js, want) {
			t.Errorf("payload %s missing %s", js, want)
		}
	}
}

// TestToInputRichMessageThinking asserts the programmatic Thinking block maps to
// a "thinking" wire block carrying the reasoning text.
func TestToInputRichMessageThinking(t *testing.T) {
	m := rich.Message{Blocks: []rich.Block{rich.Thinking{Text: "step 1\nstep 2"}}}
	got := toInputRichMessage(m)
	if len(got.Blocks) != 1 || got.Blocks[0].Type != blockThinking {
		t.Fatalf("blocks = %+v, want one thinking block", got.Blocks)
	}
	if got.Blocks[0].Reasoning != "step 1\nstep 2" {
		t.Errorf("reasoning = %q, want the joined thoughts", got.Blocks[0].Reasoning)
	}
}

// TestToInputRichMessageEmpty asserts an empty IR serializes to an empty block
// list without panicking.
func TestToInputRichMessageEmpty(t *testing.T) {
	got := toInputRichMessage(rich.Message{})
	if len(got.Blocks) != 0 {
		t.Errorf("empty message produced %d blocks, want 0", len(got.Blocks))
	}
}
