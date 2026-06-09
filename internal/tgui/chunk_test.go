//nolint:testpackage // intentionally whitebox to test unexported tgui chunking internals
package tgui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// assertChunksValid checks the invariants every chunking must hold: no chunk
// over the limit, every rune preserved in order, and no chunk splits a rune.
func assertChunksValid(t *testing.T, in string, chunks []string, limit, joinSep int) {
	t.Helper()
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > limit {
			t.Fatalf("chunk %d over limit: %d > %d", i, n, limit)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d split a rune (invalid utf8)", i)
		}
	}
	// Reassembling with the consumed separators (one rune per boundary) must
	// reproduce the original run of non-separator content. We verify a weaker but
	// solid property: concatenating chunks yields the input minus exactly the
	// boundary separators removed, so total runes are conserved.
	got := 0
	for _, c := range chunks {
		got += utf8.RuneCountInString(c)
	}
	want := utf8.RuneCountInString(in) - joinSep*(len(chunks)-1)
	if len(chunks) <= 1 {
		want = utf8.RuneCountInString(in)
	}
	if got != want && joinSep == 0 {
		t.Fatalf("rune count not conserved: got %d want %d", got, want)
	}
}

func TestChunkUnderLimit(t *testing.T) {
	in := strings.Repeat("x", 100)
	chunks := Chunk(in)
	if len(chunks) != 1 || chunks[0] != in {
		t.Fatalf("expected single chunk, got %d", len(chunks))
	}
}

func TestChunkExactlyLimit(t *testing.T) {
	in := strings.Repeat("x", TelegramMaxMessage)
	chunks := Chunk(in)
	if len(chunks) != 1 {
		t.Fatalf("exactly %d runes should be 1 chunk, got %d", TelegramMaxMessage, len(chunks))
	}
	if utf8.RuneCountInString(chunks[0]) != TelegramMaxMessage {
		t.Fatalf("chunk length changed")
	}
}

func TestChunkOverLimitHardSplit(t *testing.T) {
	// A single unbroken line longer than the limit: must hard-split with no
	// chunk over the limit.
	in := strings.Repeat("x", TelegramMaxMessage+50)
	chunks := Chunk(in)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	assertChunksValid(t, in, chunks, TelegramMaxMessage, 0)
	if strings.Join(chunks, "") != in {
		t.Fatal("hard split lost or reordered content")
	}
}

func TestChunkBreaksOnNewline(t *testing.T) {
	// Build content just over the limit with a newline near the end of the first
	// budget so the split lands on the newline, not mid-word.
	first := strings.Repeat("a", TelegramMaxMessage-10)
	second := strings.Repeat("b", 40)
	in := first + "\n" + second
	chunks := ChunkSize(in, TelegramMaxMessage)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != first {
		t.Fatalf("did not break on newline; first chunk = %q…", chunks[0][:20])
	}
	if chunks[1] != second {
		t.Fatalf("second chunk wrong: %q", chunks[1])
	}
}

func TestChunkBreaksOnSpace(t *testing.T) {
	first := strings.Repeat("a", TelegramMaxMessage-10)
	second := strings.Repeat("b", 40)
	in := first + " " + second
	chunks := ChunkSize(in, TelegramMaxMessage)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != first || chunks[1] != second {
		t.Fatalf("did not break on space: %q / %q", chunks[0][len(chunks[0])-5:], chunks[1][:5])
	}
}

func TestChunkMultiByteRunesNotSplit(t *testing.T) {
	// Each "🦆" is 4 bytes / 1 rune. A long unbroken run of them forces a hard
	// split that must land on a rune boundary.
	in := strings.Repeat("🦆", TelegramMaxMessage+100)
	chunks := ChunkSize(in, TelegramMaxMessage)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d split a multi-byte rune", i)
		}
		if n := utf8.RuneCountInString(c); n > TelegramMaxMessage {
			t.Fatalf("chunk %d over limit: %d", i, n)
		}
	}
	if strings.Join(chunks, "") != in {
		t.Fatal("multi-byte content not preserved")
	}
}

func TestChunkSmallLimitWordBreaks(t *testing.T) {
	in := "alpha beta gamma delta epsilon"
	chunks := ChunkSize(in, 12)
	for _, c := range chunks {
		if utf8.RuneCountInString(c) > 12 {
			t.Fatalf("chunk over small limit: %q", c)
		}
	}
	// Reassemble; spaces at break points are consumed, so words must all appear.
	joined := strings.Join(chunks, " ")
	if joined != in {
		t.Fatalf("word-broken reassembly mismatch: %q", joined)
	}
}

func TestChunkLeadingSeparatorNoEmptyChunk(t *testing.T) {
	// Content that begins with a separator immediately before an over-limit,
	// unbroken token. A naive LastIndexByte break at offset 0 would emit an empty
	// head chunk (Telegram rejects empty SendMessage). Cover both newline and
	// space leading separators.
	for _, sep := range []string{"\n", " "} {
		in := sep + strings.Repeat("x", TelegramMaxMessage+50)
		chunks := ChunkSize(in, TelegramMaxMessage)
		if len(chunks) < 2 {
			t.Fatalf("sep %q: expected >=2 chunks, got %d", sep, len(chunks))
		}
		for i, c := range chunks {
			if c == "" {
				t.Fatalf("sep %q: chunk %d is empty", sep, i)
			}
			if n := utf8.RuneCountInString(c); n > TelegramMaxMessage {
				t.Fatalf("sep %q: chunk %d over limit: %d", sep, i, n)
			}
		}
	}
}

func TestChunkEmpty(t *testing.T) {
	chunks := Chunk("")
	if len(chunks) != 1 || chunks[0] != "" {
		t.Fatalf("empty input should yield one empty chunk, got %v", chunks)
	}
}
