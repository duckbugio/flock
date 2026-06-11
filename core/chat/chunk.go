package chat

import (
	"strings"
	"unicode/utf8"
)

// TelegramMaxMessage is the per-chunk limit we enforce, expressed in runes.
// Telegram's true limit is 4096 UTF-16 code units, and an astral-plane rune
// counts as two of those, so a 4096-rune chunk made entirely of astral runes
// could exceed the UTF-16 limit. We accept rune count as the pragmatic budget
// here: real messages are overwhelmingly BMP text, where one rune is one UTF-16
// unit, so 4096 runes maps directly to 4096 code units.
const TelegramMaxMessage = 4096

// Chunk splits s into Telegram-safe pieces, each at most TelegramMaxMessage
// runes, never splitting a multi-byte rune. It prefers to break on newline,
// then on space, falling back to a hard rune boundary for a single
// unbroken run longer than the limit. An empty string yields a single empty
// chunk so callers always have something to send.
func Chunk(s string) []string {
	return ChunkSize(s, TelegramMaxMessage)
}

// ChunkSize is Chunk with an explicit maximum rune count per chunk, exported to
// make the boundary behavior straightforward to unit-test. A non-positive limit
// is treated as TelegramMaxMessage.
func ChunkSize(s string, limit int) []string {
	if limit <= 0 {
		limit = TelegramMaxMessage
	}
	if utf8.RuneCountInString(s) <= limit {
		return []string{s}
	}

	var chunks []string
	rest := s
	for utf8.RuneCountInString(rest) > limit {
		head, tail := splitAt(rest, limit)
		chunks = append(chunks, head)
		rest = tail
	}
	// The remaining tail is within the limit; emit it unless it is empty (which
	// happens only when the input divided evenly on a boundary we already cut).
	if rest != "" {
		chunks = append(chunks, rest)
	}
	return chunks
}

// fenceMarker is the Markdown code-fence delimiter.
const fenceMarker = "```"

// fenceInfoCap bounds the info-string length carried onto a reopened fence, so the
// per-chunk markers a split adds stay within fenceReserve.
const fenceInfoCap = 32

// fenceReserve is the rune budget held back per chunk so the close/reopen fence
// markers a mid-block split adds can never push a balanced chunk over the limit:
// a worst-case chunk gets BOTH a reopen ("```" + info + "\n") and a close
// ("\n" + "```"), i.e. <= fenceInfoCap + 8 runes. The reserve clears that.
const fenceReserve = fenceInfoCap + 16

// ChunkFenced splits s into Telegram-safe pieces like Chunk, but keeps fenced code
// blocks renderable across a split: when a break lands INSIDE a ``` block, the
// chunk that ends open is given a closing fence and the continuation a reopening
// ```<info>. Without this a split code block renders as one runaway code block on
// the rich (Markdown) path and loses its fencing on the legacy path. With no
// fenced block present the output equals Chunk's.
func ChunkFenced(s string) []string { return ChunkFencedSize(s, TelegramMaxMessage) }

// ChunkFencedSize is ChunkFenced with an explicit per-chunk rune limit.
func ChunkFencedSize(s string, limit int) []string {
	if limit <= 0 {
		limit = TelegramMaxMessage
	}
	// No fences anywhere → nothing to balance, and no need to reserve budget; behave
	// exactly like ChunkSize.
	if !strings.Contains(s, fenceMarker) {
		return ChunkSize(s, limit)
	}
	// Split with headroom so the added fence markers can't exceed the real limit.
	split := limit - fenceReserve
	if split < 1 {
		split = limit
	}
	return balanceFences(ChunkSize(s, split))
}

// balanceFences rewrites chunk seams so no chunk ends inside an open ``` fence,
// carrying the open fence's info string across the boundary.
func balanceFences(chunks []string) []string {
	out := make([]string, len(chunks))
	openInfo := "" // info string of the fence open at the current seam
	open := false  // whether a fence is open at the current seam
	for i, c := range chunks {
		var b strings.Builder
		if open {
			// Reopen the fence carried in from the previous chunk. strings.Builder
			// writes never return an error, so the results are deliberately discarded.
			_, _ = b.WriteString(fenceMarker)
			_, _ = b.WriteString(openInfo)
			_ = b.WriteByte('\n')
		}
		_, _ = b.WriteString(c)
		endOpen, endInfo := scanFences(c, open, openInfo)
		if endOpen {
			// Close the fence at the chunk boundary so this chunk renders cleanly.
			if !strings.HasSuffix(c, "\n") {
				_ = b.WriteByte('\n')
			}
			_, _ = b.WriteString(fenceMarker)
		}
		out[i] = b.String()
		open, openInfo = endOpen, endInfo
	}
	return out
}

// scanFences walks chunk line by line from the (startOpen, startInfo) fence state
// and returns the state at its end. A line whose trimmed text begins with ```
// toggles the fence: opening captures the (capped) info string after the marker,
// closing clears it.
func scanFences(chunk string, startOpen bool, startInfo string) (open bool, info string) {
	open, info = startOpen, startInfo
	for _, line := range strings.Split(chunk, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, fenceMarker) {
			continue
		}
		if open {
			open, info = false, ""
			continue
		}
		open = true
		info = strings.TrimSpace(t[len(fenceMarker):])
		if len(info) > fenceInfoCap {
			info = info[:fenceInfoCap]
		}
	}
	return open, info
}

// splitAt returns the largest prefix of s that is at most limit runes, broken on
// a newline or space boundary when one exists within the budget, plus the
// remainder with any single boundary separator consumed. When no boundary fits,
// it cuts on a hard rune boundary at exactly limit runes.
func splitAt(s string, limit int) (head, tail string) {
	// Byte offset of the (limit)-th rune: the hard cut point.
	cut := runeOffset(s, limit)
	prefix := s[:cut]

	// Prefer the last newline within the prefix, then the last space. We require
	// index > 0 so the head is never empty: a separator at offset 0 (the prefix
	// begins with a newline/space before an over-limit unbroken token) would
	// otherwise yield an empty chunk, which Telegram rejects. In that case we fall
	// through to the hard rune-boundary split.
	if i := strings.LastIndexByte(prefix, '\n'); i > 0 {
		return s[:i], s[i+1:]
	}
	if i := strings.LastIndexByte(prefix, ' '); i > 0 {
		return s[:i], s[i+1:]
	}
	// No break point: hard split on the rune boundary.
	return prefix, s[cut:]
}

// runeOffset returns the byte offset just past the n-th rune of s (or len(s) if
// s has fewer than n runes).
func runeOffset(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}
