package rich_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/chat/rich"
)

// inl builds an Inline from spans for terse expectations.
func inl(spans ...rich.Span) rich.Inline { return rich.Inline{Spans: spans} }

// text is the common case: a single plain span.
func text(s string) rich.Inline { return inl(rich.Span{Text: s}) }

func TestFromMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want rich.Message
	}{
		{
			name: "plain paragraph",
			in:   "hello world",
			want: rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: text("hello world")}}},
		},
		{
			name: "heading levels",
			in:   "### Sub heading",
			want: rich.Message{Blocks: []rich.Block{rich.Heading{Level: 3, Text: text("Sub heading")}}},
		},
		{
			name: "fenced code with lang",
			in:   "```go\nx := 1\ny := 2\n```",
			want: rich.Message{Blocks: []rich.Block{rich.Code{Lang: "go", Text: "x := 1\ny := 2"}}},
		},
		{
			name: "unterminated fence runs to EOF",
			in:   "```\nstill code",
			want: rich.Message{Blocks: []rich.Block{rich.Code{Lang: "", Text: "still code"}}},
		},
		{
			name: "bullet list",
			in:   "- a\n- b\n* c",
			want: rich.Message{Blocks: []rich.Block{rich.List{Ordered: false, Items: []rich.Inline{
				text("a"), text("b"), text("c"),
			}}}},
		},
		{
			name: "ordered list",
			in:   "1. first\n2. second",
			want: rich.Message{Blocks: []rich.Block{rich.List{Ordered: true, Items: []rich.Inline{
				text("first"), text("second"),
			}}}},
		},
		{
			name: "blockquote joins lines",
			in:   "> line one\n> line two",
			want: rich.Message{Blocks: []rich.Block{rich.Quote{Text: text("line one line two")}}},
		},
		{
			name: "link inside paragraph",
			in:   "see [docs](https://x.y) now",
			want: rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: inl(
				rich.Span{Text: "see "},
				rich.Span{Text: "docs", URL: "https://x.y"},
				rich.Span{Text: " now"},
			)}}},
		},
		{
			name: "bold and inline code",
			in:   "a **b** and `c` end",
			want: rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: inl(
				rich.Span{Text: "a "},
				rich.Span{Text: "b", Bold: true},
				rich.Span{Text: " and "},
				rich.Span{Text: "c", Code: true},
				rich.Span{Text: " end"},
			)}}},
		},
		{
			name: "underscore bold, not snake_case italic",
			in:   "use __NOW__ on foo_bar_baz",
			want: rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: inl(
				rich.Span{Text: "use "},
				rich.Span{Text: "NOW", Bold: true},
				rich.Span{Text: " on foo_bar_baz"},
			)}}},
		},
		{
			name: "pipe table",
			in:   "| H1 | H2 |\n| --- | :-: |\n| a | b |\n| c | d |",
			want: rich.Message{Blocks: []rich.Block{rich.Table{
				Header: []string{"H1", "H2"},
				Rows:   [][]string{{"a", "b"}, {"c", "d"}},
			}}},
		},
		{
			name: "pipe in prose is not a table (no separator)",
			in:   "a | b is not a table",
			want: rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: text("a | b is not a table")}}},
		},
		{
			name: "unterminated bold is literal",
			in:   "a **b c",
			want: rich.Message{Blocks: []rich.Block{rich.Paragraph{Text: text("a **b c")}}},
		},
		{
			name: "mixed document",
			in:   "# Title\n\nintro line\n\n```\ncode\n```\n\n- one\n- two\n\n> quote",
			want: rich.Message{Blocks: []rich.Block{
				rich.Heading{Level: 1, Text: text("Title")},
				rich.Paragraph{Text: text("intro line")},
				rich.Code{Lang: "", Text: "code"},
				rich.List{Ordered: false, Items: []rich.Inline{text("one"), text("two")}},
				rich.Quote{Text: text("quote")},
			}},
		},
		{
			name: "empty input yields no blocks",
			in:   "",
			want: rich.Message{},
		},
		{
			name: "whitespace-only yields no blocks",
			in:   "   \n\n  ",
			want: rich.Message{},
		},
		{
			name: "paragraph stops at list without blank line",
			in:   "intro\n- item",
			want: rich.Message{Blocks: []rich.Block{
				rich.Paragraph{Text: text("intro")},
				rich.List{Ordered: false, Items: []rich.Inline{text("item")}},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rich.FromMarkdown(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FromMarkdown(%q)\n got = %#v\nwant = %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestFromMarkdownTotal asserts the parser is total: it never panics and always
// returns at least one block for any input that contains a non-blank line.
func TestFromMarkdownTotal(t *testing.T) {
	inputs := []string{
		"", " ", "\n\n", "```", "```go", "[", "[x]", "[x](", "[x]()",
		"**", "****", "__", "`", "| |", "|---", "###", "#", "> ", "- ",
		"1.", "1. ", "a\n\n\n\nb", strings.Repeat("**a** ", 100),
		"```\n```\n```", "| a | b |\n| - |", "***bold-italic***",
	}
	for _, in := range inputs {
		t.Run("input", func(t *testing.T) {
			msg := rich.FromMarkdown(in) // must not panic
			if strings.TrimSpace(in) != "" && len(msg.Blocks) == 0 {
				t.Errorf("FromMarkdown(%q) returned no blocks for non-blank input", in)
			}
		})
	}
}

// FuzzFromMarkdown runs the parser under the fuzzer (and its seed corpus in normal
// test runs), asserting it never panics and honours the "non-blank input → ≥1
// block" invariant.
func FuzzFromMarkdown(f *testing.F) {
	for _, s := range []string{
		"# h", "```go\nx\n```", "- a\n- b", "> q", "a [l](u) b",
		"| a | b |\n| - | - |\n| 1 | 2 |", "**x** `y` __z__",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		msg := rich.FromMarkdown(in)
		if strings.TrimSpace(in) != "" && len(msg.Blocks) == 0 {
			t.Errorf("non-blank input %q produced no blocks", in)
		}
	})
}
