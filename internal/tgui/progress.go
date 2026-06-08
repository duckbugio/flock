// Package tgui holds the transport-agnostic rendering logic for the Telegram
// adapter: turning a claude event stream plus wall-clock ticks into the text of
// a live "Working… (Ns)" progress message, and splitting a final answer into
// Telegram-safe chunks. It deliberately knows nothing about the Telegram API so
// it can be unit-tested in isolation.
package tgui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/duckbugio/flock/core/claude"
)

// defaultRingSize is how many recent activity lines the progress frame shows.
const defaultRingSize = 5

// These bound the TEXT of rendered activity lines (the emoji prefix is added on
// top of each budget) so a long tool input or thought can't blow up the frame,
// while still showing enough to read. The cap is POSITIONAL: the most recent ring
// line gets the larger recentSnippetMax so the current activity is readable, and
// older lines get the tighter olderSnippetMax since they are just context. The
// caps are applied at Frame() assembly time, not when the line is stored.
const (
	recentSnippetMax = 800
	olderSnippetMax  = 400
)

// frameBudgetMax bounds the WHOLE assembled frame (header + blank + ring lines)
// in runes. It stays comfortably below TelegramMaxMessage (4096, see chunk.go) so
// a progress frame is always a single Telegram message with ~596 runes of
// headroom for markup/encoding slack; Frame() drops oldest ring lines (and, in the
// extreme, hard-truncates the most recent line) to honor it.
//
// The budget is measured on the markdown SOURCE runes; the live frame is rendered
// via MarkdownToHTML, which only adds tags/escapes, so the VISIBLE length (all
// Telegram's 4096 limit counts) is <= source length. The limit therefore still holds.
const frameBudgetMax = 3500

// Activity-line prefixes: a thought balloon for the model's text, a wrench for a
// tool call.
const (
	thoughtPrefix = "💭 "
	toolPrefix    = "🔧 "
)

// spinnerFrames animate the "Working" header. The frame is selected from the
// wall-clock elapsed seconds, so the header visibly ticks alongside the counter
// even during a long, silent tool call.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Progress accumulates a claude event stream into the text of a single editable
// "Working…" message. The elapsed header is driven by an injected clock (see
// NewProgress) rather than by events, so the counter advances even during a
// long, silent tool call (§7.2 of the rewrite plan). Progress is not safe for
// concurrent use; callers serialize Observe and Frame.
type Progress struct {
	elapsed  func() time.Duration
	ring     []string
	ringSize int
}

// NewProgress returns a Progress whose header counter reads from elapsed, the
// time since the run started. Injecting elapsed (rather than calling time.Now
// internally) keeps Frame deterministic for tests. ringSize <= 0 selects the
// default.
func NewProgress(elapsed func() time.Duration, ringSize int) *Progress {
	if ringSize <= 0 {
		ringSize = defaultRingSize
	}
	return &Progress{
		elapsed:  elapsed,
		ringSize: ringSize,
		ring:     make([]string, 0, ringSize),
	}
}

// Observe folds a single stream event into the activity ring. Terminal events
// (Result, RunError) carry no activity line and are ignored here; callers use
// Final / FinalError to render the terminal message. It reports whether the
// activity ring changed, so callers can skip a redundant edit.
func (p *Progress) Observe(e claude.Event) bool {
	line, ok := activityLine(e)
	if !ok {
		return false
	}
	p.push(line)
	return true
}

// activityLine renders the one-line activity summary for an event, or false if
// the event contributes no visible activity. The TEXT is capped at a generous
// upper bound (recentSnippetMax) when stored — enough for any ring position — and
// Frame() re-caps it tighter per recency. The emoji prefix is added on top of the
// text budget, so it is never split or counted against the cap.
func activityLine(e claude.Event) (string, bool) {
	switch e.Type {
	case claude.ToolUse:
		tool := e.Tool
		if tool == "" {
			tool = "tool"
		}
		return toolPrefix + truncateRunes(tool, recentSnippetMax), true
	case claude.Text:
		snippet := collapseWhitespace(e.Text)
		if snippet == "" {
			return "", false
		}
		return thoughtPrefix + truncateRunes(snippet, recentSnippetMax), true
	default:
		// SystemInit, ToolResult, Result, RunError: no activity line.
		return "", false
	}
}

// capLine re-caps a stored activity line's TEXT to maxText runes, preserving its
// emoji prefix (which is added on top of the budget, never split or counted). A
// line without a known prefix is capped whole.
func capLine(line string, maxText int) string {
	for _, prefix := range []string{thoughtPrefix, toolPrefix} {
		if strings.HasPrefix(line, prefix) {
			return prefix + truncateRunes(line[len(prefix):], maxText)
		}
	}
	return truncateRunes(line, maxText)
}

// push appends a line to the bounded ring, evicting the oldest when full.
func (p *Progress) push(line string) {
	if len(p.ring) == p.ringSize {
		copy(p.ring, p.ring[1:])
		p.ring[len(p.ring)-1] = line
		return
	}
	p.ring = append(p.ring, line)
}

// Frame renders the current progress message: a header line driven by the
// injected clock followed by the recent activity ring.
func (p *Progress) Frame() string {
	secs := int64(p.elapsed() / time.Second)
	if secs < 0 {
		secs = 0
	}
	spin := spinnerFrames[secs%int64(len(spinnerFrames))]
	header := fmt.Sprintf("%s Working… (%ds)", spin, secs)
	if len(p.ring) == 0 {
		return header
	}

	// Re-cap each ring line by recency: the last (most recent) line gets the
	// generous recentSnippetMax, all earlier lines the tighter olderSnippetMax.
	lines := make([]string, len(p.ring))
	for i, line := range p.ring {
		maxText := olderSnippetMax
		if i == len(p.ring)-1 {
			maxText = recentSnippetMax
		}
		lines[i] = capLine(line, maxText)
	}

	// Enforce the overall frame budget. assemble joins the header (always kept),
	// a blank separator, and the given ring lines; its rune count must not exceed
	// frameBudgetMax. Drop the OLDEST lines first until it fits.
	assemble := func(ringLines []string) string {
		var b strings.Builder
		b.WriteString(header)
		// A blank line sets the activity ("thoughts") apart from the Working header.
		b.WriteString("\n")
		for _, line := range ringLines {
			b.WriteByte('\n')
			b.WriteString(line)
		}
		return b.String()
	}
	for len(lines) > 1 && utf8.RuneCountInString(assemble(lines)) > frameBudgetMax {
		lines = lines[1:]
	}

	frame := assemble(lines)
	if utf8.RuneCountInString(frame) <= frameBudgetMax {
		return frame
	}

	// A single surviving line (the most recent) still blows the budget on its own.
	// Hard-truncate it so the frame is always <= frameBudgetMax, but never emit an
	// empty frame: keep the header + a blank + the truncated line. The header, the
	// two separating newlines, and the line's emoji prefix all ride on top of the
	// text budget, so subtract them so capLine's TEXT cap keeps the whole frame in
	// bounds.
	overhead := utf8.RuneCountInString(header) + 2
	for _, prefix := range []string{thoughtPrefix, toolPrefix} {
		if strings.HasPrefix(lines[0], prefix) {
			overhead += utf8.RuneCountInString(prefix)
			break
		}
	}
	// The header is tiny (≈20 runes) and frameBudgetMax is in the thousands, so
	// budget is always comfortably positive; the clamp is pure defense so a future
	// outsized header can never make capLine's budget non-positive.
	budget := frameBudgetMax - overhead
	if budget < 1 {
		budget = 1
	}
	return assemble([]string{capLine(lines[0], budget)})
}

// Final renders the terminal message text for a successful run result. A
// result that carries no text (e.g. an error subtype with an empty body) yields
// a short placeholder so the user is never left with an empty message.
func Final(res *claude.RunResult) string {
	if res == nil {
		return "(no result)"
	}
	text := strings.TrimSpace(res.Text)
	if res.IsError {
		if text == "" {
			text = "the run ended with an error"
		}
		return "⚠️ " + text
	}
	if text == "" {
		return "(empty response)"
	}
	return text
}

// FinalError renders the terminal message text when a run fails without a
// result envelope (a RunError event).
func FinalError(err error) string {
	if err == nil {
		return "⚠️ the run failed"
	}
	return "⚠️ " + collapseWhitespace(err.Error())
}

// collapseWhitespace trims and collapses runs of whitespace to single spaces so
// multi-line text renders as a compact one-line snippet.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes shortens s to at most maxRunes runes, appending an ellipsis
// when it had to cut. It never splits a multi-byte rune.
func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes-1 {
			break
		}
		b.WriteRune(r)
		n++
	}
	b.WriteRune('…')
	return b.String()
}
