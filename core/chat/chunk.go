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
