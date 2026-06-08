package telegram

import (
	"strings"
	"testing"
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
