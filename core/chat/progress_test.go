//nolint:testpackage // intentionally whitebox to test unexported tgui progress rendering internals
package chat

import (
	"encoding/json"
	"errors"
	"strconv"
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
	if !strings.Contains(frame, "⌨️ Bash") {
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
	// 1 header + 1 blank separator + 1 "+2 earlier" indicator + 3 activity lines.
	if len(lines) != 6 {
		t.Fatalf("expected header + blank + indicator + 3 ring lines, got %d lines: %q", len(lines), frame)
	}
	// Five pushed, three shown: two scrolled off above the window.
	if !strings.Contains(frame, "+2 earlier") {
		t.Fatalf("expected +2 earlier indicator: %q", frame)
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

func TestToolUseDetailEnrichment(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    string
		wantSub  string // substring the line must contain
		wantSep  bool   // whether " · " must appear
		wantMiss string // substring the line must NOT contain (optional)
	}{
		{
			name: "read file_path", tool: "Read",
			input:   `{"file_path":"internal/config/config.go"}`,
			wantSub: "📖 Read · internal/config/config.go", wantSep: true,
		},
		{name: "edit file_path", tool: "Edit", input: `{"file_path":"main.go"}`, wantSub: "✏️ Edit · main.go", wantSep: true},
		{name: "write file_path", tool: "Write", input: `{"file_path":"out.txt"}`, wantSub: "✏️ Write · out.txt", wantSep: true},
		{
			name: "notebookedit file_path", tool: "NotebookEdit",
			input: `{"file_path":"nb.ipynb"}`, wantSub: "✏️ NotebookEdit · nb.ipynb", wantSep: true,
		},
		{name: "bash command", tool: "Bash", input: `{"command":"go test ./..."}`, wantSub: "⌨️ Bash · go test ./...", wantSep: true},
		{
			name: "grep pattern", tool: "Grep",
			input: `{"pattern":"func main","path":"core"}`, wantSub: "🔍 Grep · func main", wantSep: true,
		},
		{name: "glob pattern", tool: "Glob", input: `{"pattern":"**/*.go"}`, wantSub: "🔍 Glob · **/*.go", wantSep: true},
		{
			name: "task description", tool: "Task",
			input:   `{"description":"run the tests","subagent_type":"tester"}`,
			wantSub: "🤖 Task · run the tests", wantSep: true,
		},
		{
			name: "task subagent fallback", tool: "Task",
			input: `{"subagent_type":"tester"}`, wantSub: "🤖 Task · tester", wantSep: true,
		},
		{name: "agent description", tool: "Agent", input: `{"description":"do work"}`, wantSub: "🤖 Agent · do work", wantSep: true},
		{
			name: "webfetch url", tool: "WebFetch",
			input: `{"url":"https://example.com"}`, wantSub: "🌐 WebFetch · https://example.com", wantSep: true,
		},
		{
			name: "websearch query", tool: "WebSearch",
			input: `{"query":"golang json"}`, wantSub: "🔎 WebSearch · golang json", wantSep: true,
		},
		{name: "skill", tool: "Skill", input: `{"skill":"pdf"}`, wantSub: "🧩 Skill · pdf", wantSep: true},
		{
			name: "toolsearch query", tool: "ToolSearch",
			input: `{"query":"search this"}`, wantSub: "🔎 ToolSearch · search this", wantSep: true,
		},
		// Case-insensitive tool match (emoji prefix resolves on the lowercased name).
		{name: "lowercase tool name", tool: "read", input: `{"file_path":"a.go"}`, wantSub: "📖 read · a.go", wantSep: true},
		// Fallbacks to name-only (no separator). An unknown tool also falls back to
		// the generic wrench prefix.
		{
			name: "unknown tool", tool: "MysteryTool",
			input: `{"file_path":"a.go"}`, wantSub: "🔧 MysteryTool", wantSep: false, wantMiss: " · ",
		},
		{name: "malformed input", tool: "Read", input: `{not json`, wantSub: "📖 Read", wantSep: false, wantMiss: " · "},
		{name: "empty input", tool: "Bash", input: ``, wantSub: "⌨️ Bash", wantSep: false, wantMiss: " · "},
		{name: "missing field", tool: "Read", input: `{"other":"x"}`, wantSub: "📖 Read", wantSep: false, wantMiss: " · "},
		{name: "non-string field", tool: "Read", input: `{"file_path":123}`, wantSub: "📖 Read", wantSep: false, wantMiss: " · "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var elapsed time.Duration
			p := NewProgress(fakeClock(&elapsed), 5)
			p.Observe(claude.Event{Type: claude.ToolUse, Tool: tc.tool, ToolInput: []byte(tc.input)})
			frame := p.Frame()
			if !strings.Contains(frame, tc.wantSub) {
				t.Fatalf("frame %q does not contain %q", frame, tc.wantSub)
			}
			if tc.wantSep && !strings.Contains(frame, " · ") {
				t.Fatalf("expected separator in %q", frame)
			}
			if tc.wantMiss != "" && strings.Contains(frame, tc.wantMiss) {
				t.Fatalf("frame %q unexpectedly contains %q", frame, tc.wantMiss)
			}
		})
	}
}

func TestToolLinePrefixPerTool(t *testing.T) {
	cases := map[string]string{
		"Read":         "📖 ",
		"Edit":         "✏️ ",
		"Write":        "✏️ ",
		"NotebookEdit": "✏️ ",
		"Bash":         "⌨️ ",
		"Grep":         "🔍 ",
		"Glob":         "🔍 ",
		"Task":         "🤖 ",
		"Agent":        "🤖 ",
		"WebFetch":     "🌐 ",
		"WebSearch":    "🔎 ",
		"ToolSearch":   "🔎 ",
		"Skill":        "🧩 ",
		// Unknown and empty tool names fall back to the generic wrench.
		"MysteryTool": "🔧 ",
		"":            "🔧 ",
		// Resolution is case-insensitive.
		"bash": "⌨️ ",
		"GREP": "🔍 ",
	}
	for tool, want := range cases {
		if got := toolLinePrefix(tool); got != want {
			t.Errorf("toolLinePrefix(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestToolUseDetailCollapsesMultilineCommand(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)
	p.Observe(claude.Event{
		Type:      claude.ToolUse,
		Tool:      "Bash",
		ToolInput: []byte("{\"command\":\"echo one\\n   echo two\\n\\techo three\"}"),
	})
	frame := p.Frame()
	if !strings.Contains(frame, "⌨️ Bash · echo one echo two echo three") {
		t.Fatalf("multi-line command not collapsed to one line: %q", frame)
	}
}

func TestToolUseDetailTruncatedToBudget(t *testing.T) {
	longCmd := strings.Repeat("x", toolDetailMax+200)
	input, err := json.Marshal(map[string]string{"command": longCmd})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	detail := toolDetail("Bash", input)
	if got := utf8.RuneCountInString(detail); got != toolDetailMax {
		t.Fatalf("detail rune count = %d, want %d (truncated with ellipsis)", got, toolDetailMax)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Fatalf("truncated detail should end with ellipsis: %q", detail)
	}
}

func TestToolDetailRedactsSecrets(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    string
		wantSub  string // detail must contain this (post-redaction)
		wantMiss string // detail must NOT contain this (the secret)
	}{
		{
			name: "bearer token in curl", tool: "Bash",
			input:   `{"command":"curl -H 'Authorization: Bearer abc123SECRET' https://api.example.com"}`,
			wantSub: "Bearer ***", wantMiss: "abc123SECRET",
		},
		{
			name: "token flag", tool: "Bash",
			input:   `{"command":"deploy --token=ghp_topSecretValue42"}`,
			wantSub: "token=***", wantMiss: "ghp_topSecretValue42",
		},
		{
			name: "password space-separated", tool: "Bash",
			input:   `{"command":"mysql -u root --password hunter2"}`,
			wantSub: "password ***", wantMiss: "hunter2",
		},
		{
			name: "api_key query param", tool: "WebFetch",
			input:   `{"url":"https://api.example.com/v1?api_key=KEY_LEAK_123"}`,
			wantSub: "api_key=***", wantMiss: "KEY_LEAK_123",
		},
		{
			name: "url userinfo credentials", tool: "Bash",
			// Split so the fixture URL isn't a single literal gosec flags as a real credential.
			input:   `{"command":"git clone https://alice:` + `s3cr3t@github.com/org/repo.git"}`,
			wantSub: "https://***@github.com", wantMiss: "s3cr3t",
		},
		{
			name: "auth bound by equals redacted", tool: "Bash",
			input:   `{"command":"deploy --auth=topSecretValue"}`,
			wantSub: "auth=***", wantMiss: "topSecretValue",
		},
		// Env-var-assignment shape (UPPER/snake_case) — the most common way a real
		// secret appears in a shell command. The keyword is a segment of a longer
		// identifier, so a bare \b would miss it ('_' is a word char).
		{
			name: "env var token assignment", tool: "Bash",
			input:   `{"command":"export GITHUB_TOKEN=ghp_realSecretValue123"}`,
			wantSub: "GITHUB_TOKEN=***", wantMiss: "ghp_realSecretValue123",
		},
		{
			name: "env var secret access key", tool: "Bash",
			input:   `{"command":"AWS_SECRET_ACCESS_KEY=AKIArealLeak0001 aws s3 ls"}`,
			wantSub: "AWS_SECRET_ACCESS_KEY=***", wantMiss: "AKIArealLeak0001",
		},
		{
			name: "env var db password", tool: "Bash",
			input:   `{"command":"DB_PASSWORD=hunter2 ./run"}`,
			wantSub: "DB_PASSWORD=***", wantMiss: "hunter2",
		},
		{
			name: "snake_case auth assignment", tool: "Bash",
			input:   `{"command":"deploy --service_auth=topSecretValue"}`,
			wantSub: "service_auth=***", wantMiss: "topSecretValue",
		},
		{
			name: "url password containing at sign", tool: "Bash",
			input:   `{"command":"git clone https://bob:` + `my@ssPhrase@example.com/r.git"}`,
			wantSub: "https://***@example.com", wantMiss: "my@ssPhrase",
		},
		// A quoted secret containing spaces must be masked as a whole; a bare \S+
		// value would stop at the first space and leak the tail.
		{
			name: "double-quoted multiword secret", tool: "Bash",
			input:   `{"command":"mysql --password \"hunter 2 spaces\""}`,
			wantSub: "password ***", wantMiss: "spaces",
		},
		{
			name: "single-quoted multiword token", tool: "Bash",
			input:   `{"command":"deploy --token='ghp abc def'"}`,
			wantSub: "token=***", wantMiss: "abc def",
		},
		{
			name: "clean command untouched", tool: "Bash",
			input:   `{"command":"go test ./..."}`,
			wantSub: "go test ./...",
		},
		// Redaction is scoped to CLI-shaped fields (command/url). A search pattern
		// or other non-command field is shown verbatim — it is not a command string,
		// so masking it would only mangle benign content.
		{
			name: "grep pattern not redacted", tool: "Grep",
			input:   `{"pattern":"token = nil"}`,
			wantSub: "token = nil", wantMiss: "***",
		},
		{
			name: "task description not redacted", tool: "Task",
			input:   `{"description":"rotate the api_key in config"}`,
			wantSub: "rotate the api_key in config", wantMiss: "***",
		},
		// Benign uses of the loose keywords must stay fully readable (no over-masking
		// when "auth" is not bound to a value by ':'/'=' and "basic"/"bearer" precede
		// a short ordinary word rather than a credential token).
		{
			name: "auth as path segment untouched", tool: "Bash",
			input:   `{"command":"go test ./auth ./config"}`,
			wantSub: "go test ./auth ./config", wantMiss: "***",
		},
		{
			name: "auth in cd chain untouched", tool: "Bash",
			input:   `{"command":"cd internal/auth && go build"}`,
			wantSub: "cd internal/auth && go build", wantMiss: "***",
		},
		{
			name: "basic auth words untouched", tool: "Bash",
			input:   `{"command":"echo basic auth check"}`,
			wantSub: "basic auth check", wantMiss: "***",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := toolDetail(tc.tool, []byte(tc.input))
			if !strings.Contains(detail, tc.wantSub) {
				t.Fatalf("detail %q does not contain %q", detail, tc.wantSub)
			}
			if tc.wantMiss != "" && strings.Contains(detail, tc.wantMiss) {
				t.Fatalf("detail %q leaked secret %q", detail, tc.wantMiss)
			}
		})
	}
}

func TestToolDetailNonObjectJSON(t *testing.T) {
	// Valid JSON that is not an object unmarshals into map[string]any with an
	// error, so the detail falls back to "" (bare tool name).
	for _, in := range []string{`"just a string"`, `[1,2,3]`, `42`} {
		if got := toolDetail("Read", []byte(in)); got != "" {
			t.Fatalf("toolDetail(%q) = %q, want empty", in, got)
		}
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

// TestFrameBudgetDropsOldestLines exercises the drop-oldest loop in Frame(): with a
// ring larger than the default, the positionally-capped lines together exceed
// frameBudgetMax, so Frame() must drop the OLDEST lines until the frame fits while
// keeping the most recent ones. (At the default ring size of 5 the per-line caps
// alone keep the frame well under budget, so the loop never triggers there.)
func TestFrameBudgetDropsOldestLines(t *testing.T) {
	var elapsed time.Duration
	const ring = 20
	p := NewProgress(fakeClock(&elapsed), ring)
	// Each line is a max-length older snippet tagged with its index at the FRONT, so
	// the tag survives end-truncation and lets us tell which lines were kept.
	for i := 0; i < ring; i++ {
		p.push(thoughtPrefix + "L" + strconv.Itoa(i) + " " + strings.Repeat("x", olderSnippetMax))
	}
	frame := p.Frame()
	if n := utf8.RuneCountInString(frame); n > frameBudgetMax {
		t.Fatalf("frame exceeded budget: %d runes (max %d)", n, frameBudgetMax)
	}
	// The drop loop must have run: fewer activity lines than pushed.
	kept := strings.Count(frame, thoughtPrefix)
	if kept >= ring {
		t.Fatalf("drop loop did not run: kept %d of %d lines", kept, ring)
	}
	if kept < 1 {
		t.Fatal("frame kept no activity lines")
	}
	// Newest survives, oldest is dropped.
	if !strings.Contains(frame, "L"+strconv.Itoa(ring-1)+" ") {
		t.Fatalf("most recent line was dropped: %.80q", frame)
	}
	if strings.Contains(frame, "L0 ") {
		t.Fatalf("oldest line should have been dropped first: %.80q", frame)
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{-5, "0s"},
		{0, "0s"},
		{45, "45s"},
		{60, "1m 00s"},
		{185, "3m 05s"},
		{1281, "21m 21s"},
		{3600, "1h 00m 00s"},
		{3725, "1h 02m 05s"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.secs); got != tc.want {
			t.Errorf("formatElapsed(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

// TestFormatElapsedInHeader confirms the humanized form reaches the rendered header.
func TestFormatElapsedInHeader(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5)
	elapsed = 1281 * time.Second
	if got := p.Frame(); !strings.Contains(got, "Working… (21m 21s)") {
		t.Fatalf("header not humanized: %q", got)
	}
}

// TestElidedIndicatorHiddenWhenAllShown asserts no "+N earlier" line appears while
// the number of pushed lines is within the visible ring.
func TestElidedIndicatorHiddenWhenAllShown(t *testing.T) {
	var elapsed time.Duration
	const ring = 5
	p := NewProgress(fakeClock(&elapsed), ring)
	for i := 0; i < ring; i++ {
		p.Observe(claude.Event{Type: claude.ToolUse, Tool: "Bash"})
	}
	if frame := p.Frame(); strings.Contains(frame, "earlier") {
		t.Fatalf("indicator shown when nothing elided: %q", frame)
	}
}

// TestElidedIndicatorShownWhenEvicted asserts that pushing more than ringSize lines
// renders "+K earlier" as the FIRST activity line, with the last ringSize lines kept.
func TestElidedIndicatorShownWhenEvicted(t *testing.T) {
	var elapsed time.Duration
	const ring = 5
	const extra = 37
	p := NewProgress(fakeClock(&elapsed), ring)
	for i := 0; i < ring+extra; i++ {
		p.Observe(claude.Event{Type: claude.ToolUse, Tool: "Bash"})
	}
	frame := p.Frame()
	if !strings.Contains(frame, "+"+strconv.Itoa(extra)+" earlier") {
		t.Fatalf("expected +%d earlier indicator: %q", extra, frame)
	}
	// The indicator must be the FIRST line of the activity block: header, blank,
	// then the indicator.
	lines := strings.Split(frame, "\n")
	if len(lines) < 3 || lines[2] != "+"+strconv.Itoa(extra)+" earlier" {
		t.Fatalf("indicator not first activity line: %q", frame)
	}
	// Exactly ringSize activity lines are kept below the indicator.
	if kept := strings.Count(frame, toolLinePrefix("Bash")); kept != ring {
		t.Fatalf("kept %d activity lines, want %d", kept, ring)
	}
}

// TestElidedIndicatorCountsBudgetDrops asserts N reflects lines dropped by the frame
// budget loop, not just ring eviction: with a large ring of long lines the budget
// loop drops some shown lines, and each drop must raise N by one.
func TestElidedIndicatorCountsBudgetDrops(t *testing.T) {
	var elapsed time.Duration
	const ring = 20
	p := NewProgress(fakeClock(&elapsed), ring)
	for i := 0; i < ring; i++ {
		p.push(thoughtPrefix + "L" + strconv.Itoa(i) + " " + strings.Repeat("x", olderSnippetMax))
	}
	frame := p.Frame()
	if n := utf8.RuneCountInString(frame); n > frameBudgetMax {
		t.Fatalf("frame exceeded budget: %d runes (max %d)", n, frameBudgetMax)
	}
	kept := strings.Count(frame, thoughtPrefix)
	if kept >= ring {
		t.Fatalf("budget loop did not drop any lines: kept %d of %d", kept, ring)
	}
	// total == ring; hidden == total - kept, and that must be the rendered N.
	hidden := ring - kept
	if !strings.Contains(frame, "+"+strconv.Itoa(hidden)+" earlier") {
		t.Fatalf("indicator N=%d not reflected after budget drops: %.80q", hidden, frame)
	}
	if hidden <= 0 {
		t.Fatalf("expected some lines hidden, got N=%d", hidden)
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

	// Diagnostic: an empty-bodied error Result surfaces the subtype so the cause
	// isn't fully opaque.
	withSub := Final(&claude.RunResult{Text: "", IsError: true, Subtype: "error_max_turns"})
	if !strings.Contains(withSub, "error_max_turns") {
		t.Fatalf("empty error result should include the subtype: %q", withSub)
	}
	// With no subtype the plain fallback is kept (no empty parentheses).
	if empty != "⚠️ the run ended with an error" {
		t.Fatalf("empty error result without subtype should use the plain fallback: %q", empty)
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
