//nolint:testpackage // whitebox: tests the unexported rich wire type
package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRichMarkdownCarriesText asserts the assistant's Markdown is passed straight
// through into InputRichMessage.markdown.
func TestRichMarkdownCarriesText(t *testing.T) {
	const md = "# Title\n\nhello **world**\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	got := richMarkdown(md)
	if got.Markdown != md {
		t.Errorf("Markdown = %q, want the input verbatim", got.Markdown)
	}
	if got.HTML != "" {
		t.Errorf("HTML = %q, want empty (exactly one of markdown/html)", got.HTML)
	}
	if !got.SkipEntityDetection {
		t.Error("SkipEntityDetection = false, want true (parity with legacy: explicit links only)")
	}
}

// TestInputRichMessageJSONShape asserts the wire payload carries "markdown" plus
// skip_entity_detection (true), and omits the empty alternative fields.
func TestInputRichMessageJSONShape(t *testing.T) {
	b, err := json.Marshal(richMarkdown("hi **there**"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"markdown":"hi **there**"`) {
		t.Errorf("payload %s missing markdown field", js)
	}
	if !strings.Contains(js, `"skip_entity_detection":true`) {
		t.Errorf("payload %s missing skip_entity_detection:true", js)
	}
	for _, absent := range []string{`"html"`, `"is_rtl"`} {
		if strings.Contains(js, absent) {
			t.Errorf("payload %s should omit empty %s", js, absent)
		}
	}
}
