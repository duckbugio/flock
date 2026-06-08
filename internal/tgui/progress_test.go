package tgui

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/duckbugio/flock/core/claude"
)

// fakeClock returns a controllable elapsed function for deterministic frames.
func fakeClock(d *time.Duration) func() time.Duration {
	return func() time.Duration { return *d }
}

func TestFrameCounterDrivenByClockNotEvents(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)

	// No events observed yet: the counter still advances purely from the clock,
	// proving it is wall-clock driven (the §7.2 frozen-counter fix).
	elapsed = 0
	if got, want := p.Frame(), spinnerFrames[0]+" Working… (0s)"; got != want {
		t.Fatalf("at 0s: got %q, want %q", got, want)
	}
	elapsed = 7 * time.Second
	if got, want := p.Frame(), spinnerFrames[7]+" Working… (7s)"; got != want {
		t.Fatalf("at 7s with no events: got %q, want %q", got, want)
	}

	// A single tool_use, then a long silent gap: the counter must keep climbing
	// even though no further events arrived.
	p.Observe(claude.Event{Type: claude.ToolUse, Tool: "Bash"})
	elapsed = 42 * time.Second
	frame := p.Frame()
	if !strings.Contains(frame, "Working… (42s)") {
		t.Fatalf("counter did not advance during silent tool call: %q", frame)
	}
	if !strings.Contains(frame, "🔧 Bash") {
		t.Fatalf("tool activity not shown: %q", frame)
	}
}

func TestActivityRingBounded(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 3)

	for _, tool := range []string{"Read", "Grep", "Edit", "Bash", "Write"} {
		p.Observe(claude.Event{Type: claude.ToolUse, Tool: tool})
	}
	frame := p.Frame()
	lines := strings.Split(frame, "\n")
	// 1 header + 1 blank separator + at most 3 activity lines.
	if len(lines) != 5 {
		t.Fatalf("expected header + blank + 3 ring lines, got %d lines: %q", len(lines), frame)
	}
	// Oldest two ("Read", "Grep") evicted; newest three retained in order.
	if !strings.Contains(frame, "Edit") || !strings.Contains(frame, "Bash") || !strings.Contains(frame, "Write") {
		t.Fatalf("ring missing recent tools: %q", frame)
	}
	if strings.Contains(frame, "Read") || strings.Contains(frame, "Grep") {
		t.Fatalf("ring did not evict oldest tools: %q", frame)
	}
}

func TestObserveTextAndIgnoredEvents(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)

	if p.Observe(claude.Event{Type: claude.SystemInit, SessionID: "s1"}) {
		t.Fatal("SystemInit should not change the ring")
	}
	if p.Observe(claude.Event{Type: claude.ToolResult}) {
		t.Fatal("ToolResult should not change the ring")
	}
	if p.Observe(claude.Event{Type: claude.Text, Text: "   \n\t "}) {
		t.Fatal("whitespace-only text should not change the ring")
	}
	if !p.Observe(claude.Event{Type: claude.Text, Text: "hello\nthere   world"}) {
		t.Fatal("non-empty text should change the ring")
	}
	frame := p.Frame()
	if !strings.Contains(frame, "hello there world") {
		t.Fatalf("text snippet not collapsed/shown: %q", frame)
	}
}

func TestActivitySnippetTruncated(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)
	// A single (hence most-recent) line longer than recentSnippetMax is capped at
	// recentSnippetMax; the emoji prefix rides on top of that text budget.
	long := strings.Repeat("a", recentSnippetMax+200)
	p.Observe(claude.Event{Type: claude.Text, Text: long})
	frame := p.Frame()
	prefixRunes := utf8.RuneCountInString(thoughtPrefix)
	var sawActivity bool
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, thoughtPrefix) {
			continue
		}
		sawActivity = true
		if n := utf8.RuneCountInString(line); n > recentSnippetMax+prefixRunes {
			t.Fatalf("snippet exceeded max: %d runes (cap %d + prefix %d)", n, recentSnippetMax, prefixRunes)
		}
	}
	if !sawActivity {
		t.Fatalf("no thought activity line found: %q", frame)
	}
	if !strings.Contains(frame, "…") {
		t.Fatalf("expected ellipsis on truncated snippet: %q", frame)
	}
}

// TestPositionalSnippetCaps asserts the per-line cap is positional by recency:
// the most-recent ring line gets recentSnippetMax (so a line between olderSnippetMax
// and recentSnippetMax stays untruncated when newest) while an older line of the
// same length is truncated to olderSnippetMax.
func TestPositionalSnippetCaps(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)

	// Length is longer than olderSnippetMax but shorter than recentSnippetMax, so
	// it is untruncated only while it is the most-recent line.
	mid := olderSnippetMax + 100
	older := strings.Repeat("o", mid)
	newer := strings.Repeat("n", mid)
	p.Observe(claude.Event{Type: claude.Text, Text: older})
	p.Observe(claude.Event{Type: claude.Text, Text: newer})

	prefixRunes := utf8.RuneCountInString(thoughtPrefix)
	var olderLine, newerLine string
	for _, line := range strings.Split(p.Frame(), "\n") {
		switch {
		case strings.HasPrefix(line, thoughtPrefix+"o"):
			olderLine = line
		case strings.HasPrefix(line, thoughtPrefix+"n"):
			newerLine = line
		}
	}
	if olderLine == "" || newerLine == "" {
		t.Fatalf("missing ring lines: older=%q newer=%q", olderLine, newerLine)
	}

	// The newer (most-recent) line fits within recentSnippetMax untruncated.
	if n := utf8.RuneCountInString(newerLine) - prefixRunes; n != mid {
		t.Fatalf("most-recent line truncated: %d text runes, want %d (no ellipsis)", n, mid)
	}
	if strings.Contains(newerLine, "…") {
		t.Fatalf("most-recent line should not be truncated: %q", newerLine)
	}
	// The older line is capped to olderSnippetMax and shows an ellipsis.
	if n := utf8.RuneCountInString(olderLine) - prefixRunes; n != olderSnippetMax {
		t.Fatalf("older line text = %d runes, want olderSnippetMax %d", n, olderSnippetMax)
	}
	if !strings.Contains(olderLine, "…") {
		t.Fatalf("older line should be truncated with an ellipsis: %q", olderLine)
	}
}

// TestFrameBudgetNeverExceedsLimit pushes many maximal-length lines and asserts the
// assembled frame stays within frameBudgetMax (and therefore below TelegramMaxMessage).
func TestFrameBudgetNeverExceedsLimit(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)
	for i := 0; i < 5; i++ {
		p.Observe(claude.Event{Type: claude.Text, Text: strings.Repeat("x", recentSnippetMax+500)})
	}
	frame := p.Frame()
	if n := utf8.RuneCountInString(frame); n > frameBudgetMax {
		t.Fatalf("frame exceeded budget: %d runes (max %d)", n, frameBudgetMax)
	}
	if utf8.RuneCountInString(frame) >= TelegramMaxMessage {
		t.Fatalf("frame not below Telegram limit %d", TelegramMaxMessage)
	}
}

// TestSingleOversizedLineHardTruncated covers the extreme: one line alone far
// larger than frameBudgetMax must be hard-truncated so the frame still contains the
// header + that line, is non-empty, and stays within budget. We push a pre-built
// oversized ring entry directly (same package) because the public Observe path caps
// stored text at recentSnippetMax, which is below the frame budget by design.
func TestSingleOversizedLineHardTruncated(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)
	p.push(thoughtPrefix + strings.Repeat("z", frameBudgetMax+2000))
	frame := p.Frame()
	if frame == "" {
		t.Fatal("frame is empty")
	}
	if !strings.HasPrefix(frame, spinnerFrames[0]) {
		t.Fatalf("frame missing header: %.40q", frame)
	}
	if !strings.Contains(frame, thoughtPrefix) {
		t.Fatalf("frame missing the activity line: %.60q", frame)
	}
	if !strings.Contains(frame, "…") {
		t.Fatalf("oversized single line should be hard-truncated with an ellipsis")
	}
	if n := utf8.RuneCountInString(frame); n > frameBudgetMax {
		t.Fatalf("frame exceeded budget: %d runes (max %d)", n, frameBudgetMax)
	}
}

func TestFinalSuccess(t *testing.T) {
	out := Final(&claude.RunResult{Text: "  the answer is 42  ", IsError: false})
	if out != "the answer is 42" {
		t.Fatalf("got %q", out)
	}
}

func TestFinalErrorResult(t *testing.T) {
	out := Final(&claude.RunResult{Text: "max turns exceeded", IsError: true})
	if !strings.HasPrefix(out, "⚠️") || !strings.Contains(out, "max turns exceeded") {
		t.Fatalf("error result not flagged: %q", out)
	}

	empty := Final(&claude.RunResult{Text: "", IsError: true})
	if !strings.HasPrefix(empty, "⚠️") {
		t.Fatalf("empty error result not flagged: %q", empty)
	}

	if got := Final(&claude.RunResult{Text: "", IsError: false}); got != "(empty response)" {
		t.Fatalf("empty success placeholder: %q", got)
	}
	if got := Final(nil); got != "(no result)" {
		t.Fatalf("nil result: %q", got)
	}
}

func TestFinalErrorFromEvent(t *testing.T) {
	out := FinalError(errors.New("claude exited\nwith code 1"))
	if !strings.HasPrefix(out, "⚠️") || !strings.Contains(out, "claude exited with code 1") {
		t.Fatalf("got %q", out)
	}
	if got := FinalError(nil); !strings.HasPrefix(got, "⚠️") {
		t.Fatalf("nil error: %q", got)
	}
}
