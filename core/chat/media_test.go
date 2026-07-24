//nolint:testpackage // intentionally whitebox to test unexported telegram media routing internals
package chat

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocumentPrompt(t *testing.T) {
	const path = "/workspace/chat_1/uploads/123-1-ab-report.pdf"

	t.Run("with caption", func(t *testing.T) {
		got := DocumentPrompt(path, "Summarize this for me")
		if !strings.Contains(got, path) {
			t.Errorf("prompt %q missing saved path", got)
		}
		if !strings.Contains(got, "Summarize this for me") {
			t.Errorf("prompt %q missing caption", got)
		}
	})

	t.Run("no caption uses default", func(t *testing.T) {
		got := DocumentPrompt(path, "   ")
		if !strings.Contains(got, path) {
			t.Errorf("prompt %q missing saved path", got)
		}
		if !strings.Contains(got, "Please read it and respond") {
			t.Errorf("caption-less prompt %q missing default instruction", got)
		}
	})
}

func TestPhotoPrompt(t *testing.T) {
	const path = "/workspace/chat_1/uploads/123-1-ab-photo.jpg"

	t.Run("with caption", func(t *testing.T) {
		got := PhotoPrompt(path, "What breed is this dog?")
		if !strings.Contains(got, path) {
			t.Errorf("prompt %q missing saved path", got)
		}
		if !strings.Contains(got, "What breed is this dog?") {
			t.Errorf("prompt %q missing caption", got)
		}
	})

	t.Run("no caption uses default", func(t *testing.T) {
		got := PhotoPrompt(path, "")
		if !strings.Contains(got, path) {
			t.Errorf("prompt %q missing saved path", got)
		}
		if !strings.Contains(got, "image") {
			t.Errorf("caption-less photo prompt %q missing default", got)
		}
	})
}

func TestQuotedPrompt(t *testing.T) {
	const userText = "What did you mean by this?"

	t.Run("with quote and author contains both, distinguishably", func(t *testing.T) {
		got := QuotedPrompt("Alice", "the original message", userText)
		if !strings.Contains(got, "the original message") {
			t.Errorf("prompt %q missing quoted text", got)
		}
		if !strings.Contains(got, userText) {
			t.Errorf("prompt %q missing user text", got)
		}
		if !strings.Contains(got, "Alice") {
			t.Errorf("prompt %q missing author clause", got)
		}
		// The two parts are labeled so the model can tell context from instruction.
		if !strings.Contains(got, "reference data") || !strings.Contains(got, "The user's message:") {
			t.Errorf("prompt %q does not clearly separate quoted context from the user's message", got)
		}
		// The quoted context is framed FIRST, the user's operative message LAST.
		if strings.Index(got, "the original message") > strings.Index(got, userText) {
			t.Errorf("prompt %q must place the quoted context before the user's message", got)
		}
	})

	t.Run("empty author omits the clause", func(t *testing.T) {
		got := QuotedPrompt("", "the original message", userText)
		if !strings.Contains(got, "the original message") || !strings.Contains(got, userText) {
			t.Errorf("prompt %q missing quoted or user text", got)
		}
		if strings.Contains(got, " from ") {
			t.Errorf("prompt %q should not include an author clause when author is empty", got)
		}
	})

	t.Run("blank quote returns user text unchanged", func(t *testing.T) {
		for _, blank := range []string{"", "   ", "\n\t  \r\n"} {
			if got := QuotedPrompt("Alice", blank, userText); got != userText {
				t.Errorf("QuotedPrompt(author, %q, userText) = %q, want the userText unchanged", blank, got)
			}
		}
	})

	t.Run("over-cap quote is truncated but user text intact", func(t *testing.T) {
		// A multibyte quote well over the rune cap: truncation must be rune-safe and
		// must never touch the user's message.
		longQuote := strings.Repeat("документация ", 400) // ~5200 runes
		got := QuotedPrompt("Alice", longQuote, userText)
		if !strings.Contains(got, userText) {
			t.Errorf("prompt %q dropped the user text on truncation", got)
		}
		if !strings.Contains(got, quotedEllipsis) {
			t.Errorf("prompt %q missing the truncation marker", got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("prompt %q is not valid UTF-8 (truncation cut mid-rune)", got)
		}
		if strings.Contains(got, longQuote) {
			t.Errorf("prompt %q should not contain the full over-cap quote", got)
		}
	})
}

func TestPhotoMediaType(t *testing.T) {
	cases := map[string]string{
		"/u/x.jpg":     "image/jpeg",
		"/u/x.jpeg":    "image/jpeg",
		"/u/x.png":     "image/png",
		"/u/x.webp":    "image/webp",
		"/u/x.gif":     "image/gif",
		"/u/no-ext":    "image/jpeg",
		"/u/x.PNG":     "image/png",
		"/u/photo.dat": "image/jpeg",
	}
	for path, want := range cases {
		if got := photoMediaType(path); got != want {
			t.Errorf("photoMediaType(%q) = %q, want %q", path, got, want)
		}
	}
}
