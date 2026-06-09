//nolint:testpackage // intentionally whitebox to test unexported tgui HTML rendering internals
package tgui

import "testing"

func TestMarkdownToHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bold", "a **bold** b", "a <b>bold</b> b"},
		{"bold underscore", "a __bold__ b", "a <b>bold</b> b"},
		{"heading", "## Что за проект", "<b>Что за проект</b>"},
		{"heading with emoji", "## 🦆 Что за проект", "<b>🦆 Что за проект</b>"},
		{"inline code", "run `make up` now", "run <code>make up</code> now"},
		{"bold around code", "**`api/`**", "<b><code>api/</code></b>"},
		{"link", "see [docs](https://x.io/y)", `see <a href="https://x.io/y">docs</a>`},
		{"bullet dash", "- one\n- two", "• one\n• two"},
		{"escape lt gt amp", "a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		// Underscores in identifiers must survive untouched (no single-_ italic).
		{"identifier underscores", "open my_file.go and a_b_c", "open my_file.go and a_b_c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MarkdownToHTML(c.in); got != c.want {
				t.Errorf("MarkdownToHTML(%q)\n  got  %q\n  want %q", c.in, got, c.want)
			}
		})
	}
}

// TestMarkdownToHTML_CodeIsNotFormatted asserts that markdown markers and angle
// brackets INSIDE code spans are treated as literal, escaped text — never parsed
// as formatting (which would otherwise emit unbalanced tags).
func TestMarkdownToHTML_CodeIsNotFormatted(t *testing.T) {
	got := MarkdownToHTML("`**x** <tag>`")
	want := "<code>**x** &lt;tag&gt;</code>"
	if got != want {
		t.Errorf("inline code body must be literal+escaped\n  got  %q\n  want %q", got, want)
	}

	fenced := MarkdownToHTML("```go\nif a < b && c {}\n```")
	wantFenced := "<pre>if a &lt; b &amp;&amp; c {}</pre>"
	if fenced != wantFenced {
		t.Errorf("fenced code body must be literal+escaped\n  got  %q\n  want %q", fenced, wantFenced)
	}
}
