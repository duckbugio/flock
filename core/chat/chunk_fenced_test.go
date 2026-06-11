//nolint:testpackage // intentionally whitebox to test unexported fenced-chunk internals
package chat

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestChunkFencedNoFenceEqualsChunk: input without any ``` is chunked exactly like
// ChunkSize (no markers added, no budget reserved).
func TestChunkFencedNoFenceEqualsChunk(t *testing.T) {
	s := strings.Repeat("word ", 5000) // well over the limit, no fences
	got := ChunkFencedSize(s, 100)
	want := ChunkSize(s, 100)
	if len(got) != len(want) {
		t.Fatalf("chunk count = %d, want %d (must match ChunkSize when no fence)", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("chunk %d differs from ChunkSize", i)
		}
	}
}

// TestChunkFencedBalancesSplitBlock: a code block that crosses a chunk boundary is
// closed at the seam and reopened (with its info string) on the next chunk.
func TestChunkFencedBalancesSplitBlock(t *testing.T) {
	body := strings.Repeat("x = 1\n", 60) // long code body that forces a split
	s := "intro\n```go\n" + body + "```\ndone"

	chunks := ChunkFencedSize(s, 80)
	if len(chunks) < 2 {
		t.Fatalf("expected the block to split into >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		// Each chunk must have balanced fences (an even number of ``` lines).
		if n := countFenceLines(c); n%2 != 0 {
			t.Errorf("chunk %d has unbalanced fences (%d markers):\n%s", i, n, c)
		}
		if utf8.RuneCountInString(c) > 80 {
			t.Errorf("chunk %d exceeds the limit: %d runes", i, utf8.RuneCountInString(c))
		}
	}
	// The continuation chunk must reopen the go fence so its code keeps rendering.
	if !strings.Contains(chunks[1], "```go") {
		t.Errorf("continuation chunk did not reopen the ```go fence:\n%s", chunks[1])
	}
}

// TestChunkFencedWholeBlockUntouched: a complete fenced block under the limit is
// returned as a single, unchanged chunk.
func TestChunkFencedWholeBlockUntouched(t *testing.T) {
	s := "```go\nx := 1\n```"
	chunks := ChunkFencedSize(s, 4096)
	if len(chunks) != 1 || chunks[0] != s {
		t.Errorf("a whole fenced block was altered: %q", chunks)
	}
}

// TestScanFencesState walks fence toggles from a starting state.
func TestScanFencesState(t *testing.T) {
	// Opens a fence, captures the info string.
	if open, info := scanFences("```python\nprint(1)", false, ""); !open || info != "python" {
		t.Errorf("open/info = %v/%q, want true/python", open, info)
	}
	// Continues an already-open fence and closes it.
	if open, _ := scanFences("more code\n```", true, "go"); open {
		t.Error("fence should be closed after the closing ```")
	}
	// A line with no fence marker preserves the incoming state.
	if open, info := scanFences("just text", true, "go"); !open || info != "go" {
		t.Errorf("state changed by a non-fence line: %v/%q", open, info)
	}
}

func countFenceLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			n++
		}
	}
	return n
}
