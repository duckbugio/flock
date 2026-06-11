package chat

// This file holds the transport-agnostic progress rendering: turning a claude
// event stream plus wall-clock ticks into the text of a live "Working… (Ns)"
// progress message. It knows nothing about any chat platform's API so it can be
// unit-tested in isolation. Final-answer chunking lives in chunk.go.

import (
	"encoding/json"
	"fmt"
	"regexp"
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

// toolDetailMax bounds the per-tool detail (file path, command, query…) appended
// to a tool activity line, so a single long command or path can't flood the line
// before the whole composed line is re-capped at recentSnippetMax.
const toolDetailMax = 160

// toolDetailSeparator joins the tool name and its extracted detail: "🔧 Read · path".
const toolDetailSeparator = " · "

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

// separatorRunes is the rune count of the "\n\n" that separates the header from
// the activity lines in a frame.
const separatorRunes = 2

// Activity-line prefixes: a thought balloon for the model's text, a wrench for a
// tool call.
const (
	thoughtPrefix = "💭 "
	toolPrefix    = "🔧 "
)

// elidedFormat renders the "+N earlier" indicator prepended to the activity block
// when older lines have scrolled off above the visible window. It is plain English
// to match the rest of the UI ("Working…", "Stop").
const elidedFormat = "+%d earlier"

// Units used by formatElapsed to humanize the elapsed counter.
const (
	secondsPerMinute = 60
	minutesPerHour   = 60
	secondsPerHour   = secondsPerMinute * minutesPerHour
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
	// total counts every activity line ever pushed (not no-op/ignored events), so
	// Frame can show how many lines have scrolled off above the visible window.
	total int
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
		line := tool
		if detail := toolDetail(tool, e.ToolInput); detail != "" {
			line += toolDetailSeparator + detail
		}
		return toolPrefix + truncateRunes(line, recentSnippetMax), true
	case claude.Text:
		// Model "thought" text is free-form prose, not a CLI-shaped command, so it is
		// shown verbatim and deliberately NOT run through redactSecrets: the keyword
		// heuristics that are safe on a shell command ("password hunter2") would mangle
		// ordinary prose ("password reset", "token bucket", "basic understanding"). The
		// frame is purely cosmetic and the model is not expected to echo raw credentials
		// here; only the CLI-shaped tool details (above) carry that risk and are redacted.
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

// toolDetail extracts a short, human-readable detail from a tool's JSON input so a
// tool activity line can show WHAT is being read/run, not just the tool name. It is
// best-effort and purely cosmetic: any empty, malformed, or unrecognized input
// yields "" so the caller falls back to the bare tool name. The chosen value is
// whitespace-collapsed and truncated to toolDetailMax runes so one long command or
// path can't dominate the line.
func toolDetail(tool string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}

	// Pick the input field to surface BY TOOL NAME first, so an unrecognized tool
	// costs nothing: we bail out before unmarshalling. "task"/"agent" prefer
	// "description" and fall back to "subagent_type" (handled below).
	var key string
	switch strings.ToLower(tool) {
	case "read", "edit", "write", "notebookedit":
		key = "file_path"
	case "bash":
		key = "command"
	case "grep", "glob":
		key = "pattern"
	case "task", "agent":
		key = "description"
	case "webfetch":
		key = "url"
	case "websearch", "toolsearch":
		key = "query"
	case "skill":
		key = "skill"
	default:
		return ""
	}

	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}

	// strField returns args[k] only when it is actually a JSON string.
	strField := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}

	raw := strField(key)
	if raw == "" && key == "description" {
		raw = strField("subagent_type")
	}

	detail := collapseWhitespace(raw)
	// Redact obvious secret-bearing fragments BEFORE truncation, but ONLY for the
	// CLI-shaped fields that can actually carry a credential: a shell command or a
	// URL. The other fields (file paths, search patterns, queries, skill/agent
	// names) are not command strings, so masking them would only risk mangling
	// benign content ("Grep · token = nil") without protecting anything. Done
	// before truncateRunes so a secret near the cap can't survive by being cut.
	if key == "command" || key == "url" {
		detail = redactSecrets(detail)
	}
	if detail == "" {
		return ""
	}
	return truncateRunes(detail, toolDetailMax)
}

// secretRedactions masks common secret-bearing fragments in a tool detail before
// it is shown in the chat-visible progress frame. The line is purely cosmetic, so
// over-masking is fine while leaking a credential is not. Each entry keeps the
// surrounding context and replaces only the sensitive value with redactedMask.
var secretRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	// HTTP auth schemes: "Bearer <token>", "Basic <token>". The token must be at
	// least minSchemeTokenLen chars so a real credential is masked while the plain
	// English "basic auth" / "bearer of" is left readable.
	{
		regexp.MustCompile(fmt.Sprintf(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/=-]{%d,})`, minSchemeTokenLen)),
		"$1 " + redactedMask,
	},
	// Credential-ish key/value pairs: flags, query params, env assignments. A
	// strong keyword may be separated from its value by whitespace OR ':' / '='
	// (covers "--password hunter2", "token: x", "api_key=x"). The keyword is matched
	// even when it is a segment of a longer identifier — the dominant real-world
	// shape is an UPPER/snake_case env assignment ("GITHUB_TOKEN=…",
	// "AWS_SECRET_ACCESS_KEY=…", "DB_PASSWORD=…"). A bare \b would miss those because
	// '_' is a word char (no boundary before the keyword), so we allow surrounding
	// identifier chars [\w.-] on both sides and capture the whole left-hand name.
	{
		regexp.MustCompile(`(?i)\b([\w.-]*(?:token|secret|password|passwd|api[_-]?key|access[_-]?token)[\w.-]*)(\s*[:=]\s*|\s+)\S+`),
		"${1}${2}" + redactedMask,
	},
	// "auth" alone is too common in benign commands ("go test ./auth", "cd auth
	// && …") to mask on a bare space, so it ONLY redacts when bound to its value by
	// ':' / '=' ("--auth=token", "auth: x") — not by whitespace. Like the rule above
	// it tolerates an identifier prefix so "X_AUTH=…" is still caught.
	{
		regexp.MustCompile(`(?i)\b([\w.-]*auth)(\s*[:=]\s*)\S+`),
		"${1}${2}" + redactedMask,
	},
	// URL userinfo: scheme://user:pass@host -> scheme://***@host. The password run
	// extends to the LAST '@' before the path so a literal '@' inside the password
	// ("user:p@ss@host") is fully masked rather than leaving a tail visible.
	{
		regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s]+@`),
		"${1}" + redactedMask + "@",
	},
}

// minSchemeTokenLen is the shortest token the Bearer/Basic redaction will treat as
// a credential; shorter runs (e.g. the word "auth" after "basic") stay readable.
const minSchemeTokenLen = 8

// redactedMask is the placeholder substituted for a masked secret value.
const redactedMask = "***"

// redactSecrets applies secretRedactions in order, masking credential-like
// fragments while preserving the rest of the detail for context.
func redactSecrets(s string) string {
	for _, r := range secretRedactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
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

// push appends a line to the bounded ring, evicting the oldest when full. It also
// bumps total, the running count of every activity line ever pushed.
func (p *Progress) push(line string) {
	p.total++
	if len(p.ring) == p.ringSize {
		copy(p.ring, p.ring[1:])
		p.ring[len(p.ring)-1] = line
		return
	}
	p.ring = append(p.ring, line)
}

// formatElapsed humanizes an elapsed-seconds count, showing hours and minutes
// only when present and zero-padding sub-units: "45s", "21m 21s", "1h 02m 05s".
// Negative input is clamped to "0s".
func formatElapsed(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < secondsPerMinute:
		return fmt.Sprintf("%ds", secs)
	case secs < secondsPerHour:
		return fmt.Sprintf("%dm %02ds", secs/secondsPerMinute, secs%secondsPerMinute)
	default:
		h := secs / secondsPerHour
		m := (secs % secondsPerHour) / secondsPerMinute
		s := secs % secondsPerMinute
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
}

// Frame renders the current progress message: a header line driven by the
// injected clock followed by the recent activity ring.
func (p *Progress) Frame() string {
	secs := int64(p.elapsed() / time.Second)
	if secs < 0 {
		secs = 0
	}
	spin := spinnerFrames[secs%int64(len(spinnerFrames))]
	header := fmt.Sprintf("%s Working… (%s)", spin, formatElapsed(secs))
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
	// a blank separator, the "+N earlier" indicator (when lines have scrolled off
	// above the shown window), and the given ring lines; its rune count must not
	// exceed frameBudgetMax. Drop the OLDEST lines first until it fits.
	//
	// hidden is computed from the lines being SHOWN in this call (p.total minus the
	// shown count), so each dropped line raises N by one — keeping the indicator
	// self-consistent with the drop loop below.
	assemble := func(ringLines []string) string {
		var b strings.Builder
		_, _ = b.WriteString(header)
		// A blank line sets the activity ("thoughts") apart from the Working header.
		_, _ = b.WriteString("\n")
		if hidden := p.total - len(ringLines); hidden > 0 {
			_ = b.WriteByte('\n')
			_, _ = fmt.Fprintf(&b, elidedFormat, hidden)
		}
		for _, line := range ringLines {
			_ = b.WriteByte('\n')
			_, _ = b.WriteString(line)
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
	// two separating newlines, the optional "+N earlier" indicator line, and the
	// line's emoji prefix all ride on top of the text budget, so subtract them so
	// capLine's TEXT cap keeps the whole frame in bounds.
	overhead := utf8.RuneCountInString(header) + separatorRunes
	if hidden := p.total - 1; hidden > 0 {
		// The indicator line plus its leading newline.
		overhead += 1 + utf8.RuneCountInString(fmt.Sprintf(elidedFormat, hidden))
	}
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
			// Surface the result subtype (e.g. "error_max_turns") when present so the
			// cause isn't fully opaque; fall back to the plain message otherwise.
			if res.Subtype != "" {
				text = "the run ended with an error (" + res.Subtype + ")"
			} else {
				text = "the run ended with an error"
			}
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
		_, _ = b.WriteRune(r)
		n++
	}
	_, _ = b.WriteRune('…')
	return b.String()
}
