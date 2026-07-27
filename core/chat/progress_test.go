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

	"github.com/duckbugio/flock/core/agent"
)

// fakeClock returns a controllable elapsed function for deterministic frames.
func fakeClock(d *time.Duration) func() time.Duration {
	return func() time.Duration { return *d }
}

func TestFrameCounterDrivenByClockNotEvents(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")

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
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})
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
	p := NewProgress(fakeClock(&elapsed), 3, "")

	for _, tool := range []string{"Read", "Grep", "Edit", "Bash", "Write"} {
		p.Observe(agent.Event{Type: agent.ToolUse, Tool: tool})
	}
	frame := p.Frame()
	// Blocks are separated by a blank line ("\n\n"): 1 header + 1 "+2 earlier"
	// indicator + 3 activity lines (the ring is bounded to ringSize=3).
	blocks := strings.Split(frame, "\n\n")
	if len(blocks) != 5 {
		t.Fatalf("expected header + indicator + 3 ring lines, got %d blocks: %q", len(blocks), frame)
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

// TestFrameSeparatesStepsWithBlankLine asserts each activity step is set apart by a
// blank line, so the steps read as separated blocks instead of a run-together wall.
func TestFrameSeparatesStepsWithBlankLine(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Read"})
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})

	frame := p.Frame()
	// Blocks are separated by a blank line ("\n\n"): the header + one block per step.
	blocks := strings.Split(frame, "\n\n")
	if len(blocks) != 3 {
		t.Fatalf("expected header + 2 blank-line-separated steps, got %d blocks: %q", len(blocks), frame)
	}
	// Each activity block is a single line — no two steps run together inside a block.
	for _, b := range blocks[1:] {
		if strings.Contains(b, "\n") {
			t.Fatalf("steps not blank-line separated; block has an embedded newline: %q", b)
		}
	}
}

func TestObserveTextAndIgnoredEvents(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")

	if p.Observe(agent.Event{Type: agent.SystemInit, SessionID: "s1"}) {
		t.Fatal("SystemInit should not change the ring")
	}
	if p.Observe(agent.Event{Type: agent.ToolResult}) {
		t.Fatal("ToolResult should not change the ring")
	}
	if p.Observe(agent.Event{Type: agent.Text, Text: "   \n\t "}) {
		t.Fatal("whitespace-only text should not change the ring")
	}
	if !p.Observe(agent.Event{Type: agent.Text, Text: "hello\nthere   world"}) {
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
		{
			name: "bash command hidden", tool: "Bash",
			input: `{"command":"go test ./..."}`, wantSub: "⌨️ Bash", wantSep: false, wantMiss: " · ",
		},
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
			p := NewProgress(fakeClock(&elapsed), 5, "")
			p.Observe(agent.Event{Type: agent.ToolUse, Tool: tc.tool, ToolInput: []byte(tc.input)})
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

// TestToolDetailCollapsesMultilineCommand checks toolDetail collapses a multi-line
// detail to one line. Bash is no longer shown with its command in the live frame
// (see activityLine), so this exercises toolDetail directly rather than via Frame.
func TestToolDetailCollapsesMultilineCommand(t *testing.T) {
	got := toolDetail("Bash", []byte("{\"command\":\"echo one\\n   echo two\\n\\techo three\"}"), "")
	if got != "echo one echo two echo three" {
		t.Fatalf("multi-line command not collapsed to one line: %q", got)
	}
}

// TestActivityLineHidesBashCommand asserts a Bash tool_use renders as a bare
// "⌨️ Bash" line — never its command — while another tool still shows its detail.
func TestActivityLineHidesBashCommand(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash", ToolInput: []byte(`{"command":"echo secret"}`)})
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Read", ToolInput: []byte(`{"file_path":"main.go"}`)})

	frame := p.Frame()
	if !strings.Contains(frame, "⌨️ Bash") {
		t.Fatalf("Bash line missing: %q", frame)
	}
	if strings.Contains(frame, "echo secret") {
		t.Fatalf("Bash command leaked into the frame: %q", frame)
	}
	if !strings.Contains(frame, "📖 Read · main.go") {
		t.Fatalf("non-Bash tool lost its detail: %q", frame)
	}
}

func TestToolUseDetailTruncatedToBudget(t *testing.T) {
	longCmd := strings.Repeat("x", toolDetailMax+200)
	input, err := json.Marshal(map[string]string{"command": longCmd})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	detail := toolDetail("Bash", input, "")
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
			detail := toolDetail(tc.tool, []byte(tc.input), "")
			if !strings.Contains(detail, tc.wantSub) {
				t.Fatalf("detail %q does not contain %q", detail, tc.wantSub)
			}
			if tc.wantMiss != "" && strings.Contains(detail, tc.wantMiss) {
				t.Fatalf("detail %q leaked secret %q", detail, tc.wantMiss)
			}
		})
	}
}

func TestToolDetailRedactsQuotedKeySecret(t *testing.T) {
	// A secret carried in a JSON request body inside a shell command: the key is
	// bound to its value by `":"`, not a bare `:`/`=`/space. The separator must
	// tolerate the closing quote so the value is still masked. Built via Marshal to
	// avoid escaping the inner JSON by hand.
	cases := []struct {
		name string
		cmd  string
		miss string
	}{
		{
			name: "strong keyword in json body",
			cmd:  `curl -d '{"password":"hunter2json"}' https://api.example.com`,
			miss: "hunter2json",
		},
		{
			name: "json body with space after colon",
			cmd:  `curl -d '{"api_key": "KEY_LEAK_JSON"}'`,
			miss: "KEY_LEAK_JSON",
		},
		{
			name: "auth bound by quoted colon in json",
			cmd:  `curl -d '{"auth":"topSecretJson"}'`,
			miss: "topSecretJson",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"command": tc.cmd})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			detail := toolDetail("Bash", input, "")
			if strings.Contains(detail, tc.miss) {
				t.Fatalf("detail leaked JSON-body secret %q: %q", tc.miss, detail)
			}
			if !strings.Contains(detail, redactedMask) {
				t.Fatalf("detail should contain the redaction mask: %q", detail)
			}
		})
	}
}

func TestToolDetailNonObjectJSON(t *testing.T) {
	// Valid JSON that is not an object unmarshals into map[string]any with an
	// error, so the detail falls back to "" (bare tool name).
	for _, in := range []string{`"just a string"`, `[1,2,3]`, `42`} {
		if got := toolDetail("Read", []byte(in), ""); got != "" {
			t.Fatalf("toolDetail(%q) = %q, want empty", in, got)
		}
	}
}

func TestRepoRelPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		baseDir string
		want    string
	}{
		{
			name:    "under workdir relativized with repo segment kept",
			path:    "/workspace/chat_123/roost/web/src/components/SettingsForm.tsx",
			baseDir: "/workspace/chat_123",
			want:    "roost/web/src/components/SettingsForm.tsx",
		},
		{
			name:    "outside workdir unchanged",
			path:    "/etc/passwd",
			baseDir: "/workspace/chat_123",
			want:    "/etc/passwd",
		},
		{
			name:    "sibling dir would need leading dotdot so unchanged",
			path:    "/workspace/chat_999/roost/main.go",
			baseDir: "/workspace/chat_123",
			want:    "/workspace/chat_999/roost/main.go",
		},
		{
			name:    "empty baseDir unchanged",
			path:    "/workspace/chat_123/roost/web/App.tsx",
			baseDir: "",
			want:    "/workspace/chat_123/roost/web/App.tsx",
		},
		{
			name:    "non-absolute path under baseDir unchanged",
			path:    "relative/path.go",
			baseDir: "/workspace/chat_123",
			want:    "relative/path.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoRelPath(tc.path, tc.baseDir); got != tc.want {
				t.Fatalf("repoRelPath(%q, %q) = %q, want %q", tc.path, tc.baseDir, got, tc.want)
			}
		})
	}
}

func TestToolDetailFilePathRelativized(t *testing.T) {
	input, err := json.Marshal(map[string]string{
		"file_path": "/workspace/chat_123/roost/web/src/components/SettingsForm.tsx",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Under the workdir → repo-relative (repo segment kept).
	if got := toolDetail("Read", input, "/workspace/chat_123"); got != "roost/web/src/components/SettingsForm.tsx" {
		t.Fatalf("toolDetail relativized = %q, want repo-relative path", got)
	}
	// Empty baseDir → unchanged absolute path.
	if got := toolDetail("Read", input, ""); got != "/workspace/chat_123/roost/web/src/components/SettingsForm.tsx" {
		t.Fatalf("toolDetail with empty baseDir = %q, want unchanged absolute path", got)
	}
}

func TestFrameRendersFilePathRelative(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "/workspace/chat_123")
	input, err := json.Marshal(map[string]string{
		"file_path": "/workspace/chat_123/roost/web/src/components/SettingsForm.tsx",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Read", ToolInput: input})
	frame := p.Frame()
	if !strings.Contains(frame, "📖 Read · roost/web/src/components/SettingsForm.tsx") {
		t.Fatalf("frame should render repo-relative Read line, got: %q", frame)
	}
	if strings.Contains(frame, "/workspace/chat_123/") {
		t.Fatalf("frame should not contain the absolute workdir prefix, got: %q", frame)
	}
}

func TestActivitySnippetTruncated(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	// A single (hence most-recent) line longer than recentSnippetMax is capped at
	// recentSnippetMax; the emoji prefix rides on top of that text budget.
	long := strings.Repeat("a", recentSnippetMax+200)
	p.Observe(agent.Event{Type: agent.Text, Text: long})
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
	p := NewProgress(fakeClock(&elapsed), 5, "")

	// Length is longer than olderSnippetMax but shorter than recentSnippetMax, so
	// it is untruncated only while it is the most-recent line.
	mid := olderSnippetMax + 100
	older := strings.Repeat("o", mid)
	newer := strings.Repeat("n", mid)
	p.Observe(agent.Event{Type: agent.Text, Text: older})
	p.Observe(agent.Event{Type: agent.Text, Text: newer})

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
	p := NewProgress(fakeClock(&elapsed), 5, "")
	for i := 0; i < 5; i++ {
		p.Observe(agent.Event{Type: agent.Text, Text: strings.Repeat("x", recentSnippetMax+500)})
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
	p := NewProgress(fakeClock(&elapsed), 5, "")
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
	p := NewProgress(fakeClock(&elapsed), ring, "")
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
	p := NewProgress(fakeClock(&elapsed), 5, "")
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
	p := NewProgress(fakeClock(&elapsed), ring, "")
	for i := 0; i < ring; i++ {
		p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})
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
	p := NewProgress(fakeClock(&elapsed), ring, "")
	for i := 0; i < ring+extra; i++ {
		p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})
	}
	frame := p.Frame()
	if !strings.Contains(frame, "+"+strconv.Itoa(extra)+" earlier") {
		t.Fatalf("expected +%d earlier indicator: %q", extra, frame)
	}
	// The indicator must be the FIRST line of the activity block: header, blank,
	// then the indicator (carrying the clock-emoji prefix, like the activity lines).
	lines := strings.Split(frame, "\n")
	if len(lines) < 3 || lines[2] != "🕓 +"+strconv.Itoa(extra)+" earlier" {
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
	p := NewProgress(fakeClock(&elapsed), ring, "")
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

// taskEvent builds a Task ToolUse event carrying the given subagent_type, the way
// the Lead's subagent launches surface in the stream (AC1/AC2). An optional toolID
// (the first variadic arg) sets the launch's tool_use id — what an inner event later
// echoes as its ParentID, so the inner activity can be attributed back to this
// subagent (AC3). Omitting it keeps the existing no-id call sites unchanged.
func taskEvent(subagentType string, toolID ...string) agent.Event {
	e := agent.Event{
		Type:      agent.ToolUse,
		Tool:      "Task",
		ToolInput: []byte(`{"subagent_type":"` + subagentType + `"}`),
	}
	if len(toolID) > 0 {
		e.ToolID = toolID[0]
	}
	return e
}

// agentEvent builds an Agent ToolUse launch carrying the given subagent_type and
// (optionally) a tool_use id, mirroring how THIS harness spawns subagents — via the
// Agent tool, not Task (AC1/AC3). A non-empty toolID is what an inner event later
// echoes as its ParentID, so it can be attributed back to this subagent.
func agentEvent(subagentType, toolID string) agent.Event {
	return agent.Event{
		Type:      agent.ToolUse,
		Tool:      "Agent",
		ToolInput: []byte(`{"subagent_type":"` + subagentType + `"}`),
		ToolID:    toolID,
	}
}

// TestStageHeaderActiveSubagent (AC1) asserts a Task event with subagent_type
// "coder" makes the stage header show the active-stage label "coder".
func TestStageHeaderActiveSubagent(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(taskEvent("coder"))
	frame := p.Frame()
	if !strings.Contains(frame, stagePrefix) {
		t.Fatalf("stage header missing its prefix: %q", frame)
	}
	// The active label is the subagent name wrapped in the active markers.
	if !strings.Contains(frame, stageActiveLeft+"coder"+stageActiveRight) {
		t.Fatalf("active stage label not marked: %q", frame)
	}
}

// TestStageHeaderAbsentWithoutTask (AC1) asserts a chat with ZERO Task events shows
// NO stage line, and the frame is byte-identical to one rendered before this feature
// existed (header + ring only, no stage block).
func TestStageHeaderAbsentWithoutTask(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	// A non-Task tool and a thought — ordinary activity, no pipeline.
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Read", ToolInput: []byte(`{"file_path":"a.go"}`)})
	p.Observe(agent.Event{Type: agent.Text, Text: "thinking"})
	frame := p.Frame()
	if strings.Contains(frame, stagePrefix) {
		t.Fatalf("stage header rendered with no Task events: %q", frame)
	}
	// Byte-identical to a frame assembled the old way: header, then each step,
	// blank-line separated — no stage block between the header and the ring.
	want := spinnerFrames[0] + " Working… (0s)" + "\n\n" +
		"📖 Read · a.go" + "\n\n" + "💭 thinking"
	if frame != want {
		t.Fatalf("frame not byte-identical to the pre-feature layout:\n got %q\nwant %q", frame, want)
	}
}

// TestStageHeaderEmptyRingNoTask guards the empty-ring fast path: with no events at
// all the frame is exactly the bare "Working…" header (no stage block).
func TestStageHeaderEmptyRingNoTask(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	if got, want := p.Frame(), spinnerFrames[0]+" Working… (0s)"; got != want {
		t.Fatalf("empty frame not byte-identical: got %q, want %q", got, want)
	}
}

// TestStageChangesOnlyOnTask (AC2) asserts the active stage flips ONLY on a
// Tool=="Task" ToolUse: an inner, non-Task tool event (the kind a running subagent
// emits, arriving flat at the top level) must NOT change the active stage.
func TestStageChangesOnlyOnTask(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(taskEvent("planner"))
	// An inner subagent tool call (flat at the top level) — must not flip the stage.
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Read", ToolInput: []byte(`{"file_path":"x.go"}`)})
	if p.activeStage != 0 || len(p.stages) != 1 || p.stages[0] != "planner" {
		t.Fatalf("inner tool events changed the stage: active=%d stages=%v", p.activeStage, p.stages)
	}
	if !strings.Contains(p.Frame(), stageActiveLeft+"planner"+stageActiveRight) {
		t.Fatalf("planner not the active stage after inner tools: %q", p.Frame())
	}
}

// TestStageTrailFromAgentLaunches (AC1) asserts the stage trail builds when
// subagents are launched via the Agent tool (this harness's mechanism), with inner
// non-task/agent ToolUse events NOT flipping the active stage. The sequence
// Agent{coder} → (inner Bash) → Agent{reviewer} → Agent{arbiter} renders the trail
// "coder → reviewer → ▸ arbiter ◂" with the most recent one active.
func TestStageTrailFromAgentLaunches(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(agentEvent("coder", "toolu_1"))
	// An inner subagent tool call (flat at the top level) — must not advance the stage.
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash", ParentID: "toolu_1"})
	p.Observe(agentEvent("reviewer", "toolu_2"))
	p.Observe(agentEvent("arbiter", "toolu_3"))

	if got, want := strings.Join(p.stages, ","), "coder,reviewer,arbiter"; got != want {
		t.Fatalf("Agent-launch trail = %q, want %q", got, want)
	}
	if p.activeStage != 2 {
		t.Fatalf("active stage = %d, want arbiter at index 2", p.activeStage)
	}
	wantLine := stagePrefix + "coder" + stageSeparator + "reviewer" + stageSeparator +
		stageActiveLeft + "arbiter" + stageActiveRight
	if frame := p.Frame(); !strings.Contains(frame, wantLine) {
		t.Fatalf("stage line %q not in frame %q", wantLine, frame)
	}
}

// TestStageTaskAndAgentInterop (AC1) asserts the Task tool still advances the stage
// alongside Agent launches, so a mixed stream keeps working.
func TestStageTaskAndAgentInterop(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(taskEvent("planner"))
	p.Observe(agentEvent("coder", "toolu_1"))
	if got, want := strings.Join(p.stages, ","), "planner,coder"; got != want {
		t.Fatalf("mixed Task/Agent trail = %q, want %q", got, want)
	}
	if p.activeStage != 1 {
		t.Fatalf("active stage = %d, want coder at index 1", p.activeStage)
	}
}

// TestActivityLineSubagentAttribution (AC3) asserts an inner event whose ParentID
// matches a prior Agent launch's ToolID is tagged with that subagent's label at the
// HEAD of the line body — right after the emoji prefix, before the tool text — so the
// tag survives capLine's prefix-peeling and tail truncation.
func TestActivityLineSubagentAttribution(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(agentEvent("coder", "toolu_1"))
	// The coder subagent runs a Bash command; it arrives flat with ParentID == the
	// launch's ToolID, so the line is attributed to "coder".
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash", ParentID: "toolu_1"})

	frame := p.Frame()
	wantLine := "⌨️ coder" + subagentTagSeparator + "Bash"
	if !strings.Contains(frame, wantLine) {
		t.Fatalf("attributed activity line %q not in frame %q", wantLine, frame)
	}
	// The label sits AFTER the emoji prefix, not before it (so capLine's prefix-peel
	// and the activity-line emoji convention are preserved).
	if strings.Contains(frame, "coder ⌨️") {
		t.Fatalf("label leaked BEFORE the emoji prefix: %q", frame)
	}
}

// TestActivityLineNoAttributionByteIdentical (AC3/AC4) asserts an event with an
// empty ParentID (a top-level event, or a stream with no parent linkage) renders
// with NO tag — byte-identical to the same event observed with attribution machinery
// absent. The two frames must match exactly.
func TestActivityLineNoAttributionByteIdentical(t *testing.T) {
	var e1, e2 time.Duration

	// (a) A progress that never saw a subagent launch — pure top-level activity.
	bare := NewProgress(fakeClock(&e1), 5, "")
	bare.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})

	// (b) A progress that DID launch a subagent, then sees an unrelated top-level
	// event (empty ParentID): it must render identically to (a), with no tag.
	withMap := NewProgress(fakeClock(&e2), 5, "")
	withMap.Observe(agentEvent("coder", "toolu_1"))
	bareTopLevel := withMap.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})
	if !bareTopLevel {
		t.Fatal("expected the top-level Bash event to push an activity line")
	}

	// The Bash line in (b) must carry no subagent tag.
	wantBash := "⌨️ Bash"
	if frame := withMap.Frame(); !strings.Contains(frame, wantBash) {
		t.Fatalf("untagged Bash line %q not in frame %q", wantBash, frame)
	}
	if frame := withMap.Frame(); strings.Contains(frame, "⌨️ coder"+subagentTagSeparator) {
		t.Fatalf("top-level event wrongly tagged with a subagent: %q", frame)
	}
	// And the bare-progress Bash line is byte-identical to the pre-feature rendering.
	if got, want := bare.Frame(), spinnerFrames[0]+" Working… (0s)"+"\n\n"+"⌨️ Bash"; got != want {
		t.Fatalf("untagged frame not byte-identical:\n got %q\nwant %q", got, want)
	}
}

// TestActivityLineUnknownParentNoTag (AC3) asserts an event whose ParentID does NOT
// map to a known subagent launch renders untagged (graceful no-op), so a stream that
// carries parent ids we never saw a launch for never produces a bogus label.
func TestActivityLineUnknownParentNoTag(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(agentEvent("coder", "toolu_1"))
	// ParentID points at a tool_use id we never recorded — degrade to no tag.
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash", ParentID: "toolu_unknown"})
	frame := p.Frame()
	if strings.Contains(frame, subagentTagSeparator+"Bash") {
		t.Fatalf("unknown-parent event wrongly tagged: %q", frame)
	}
	if !strings.Contains(frame, "⌨️ Bash") {
		t.Fatalf("expected a plain Bash line for an unknown parent: %q", frame)
	}
}

// TestActivityLineTagSurvivesTruncation (AC3) is the load-bearing property test for
// the HEAD-anchored subagent tag: because the tag is prepended to the body (right
// after the emoji prefix), it survives both capLine's tail-truncation and the older-
// line recency re-cap to olderSnippetMax. We attribute a thought event whose body far
// exceeds olderSnippetMax (400) — a tool-detail line cannot, since toolDetail self-caps
// at toolDetailMax (160), so a thought is the only body long enough to force the 400-cap
// truncation — then push enough later lines to shove the tagged line into an OLDER
// (non-most-recent) ring slot. The rendered older line must STILL begin with the emoji
// prefix + "coder" + the tag separator (the tag survived) AND end with the ellipsis
// (the body tail was cut), proving tag-survival, not just that a short tagged line renders.
func TestActivityLineTagSurvivesTruncation(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(agentEvent("coder", "toolu_1"))

	// A long inner thought attributed to the coder subagent. Its body (after the head
	// tag) must comfortably exceed olderSnippetMax so the 400-cap actually truncates it.
	longBody := strings.Repeat("z", olderSnippetMax+300)
	if !p.Observe(agent.Event{Type: agent.Text, Text: longBody, ParentID: "toolu_1"}) {
		t.Fatal("expected the inner thought event to push an activity line")
	}
	// Sanity: the STORED body (emoji prefix peeled) really exceeds olderSnippetMax before
	// any re-cap, so the older-line truncation below is genuinely exercised.
	stored := p.ring[len(p.ring)-1]
	storedBody := strings.TrimPrefix(stored, thoughtPrefix)
	if n := utf8.RuneCountInString(storedBody); n <= olderSnippetMax {
		t.Fatalf("stored tagged body = %d runes, must exceed olderSnippetMax %d to test truncation", n, olderSnippetMax)
	}

	// Push additional lines so the tagged line leaves the most-recent slot for an OLDER
	// ring position (where it is re-capped to olderSnippetMax), while staying inside the
	// 5-slot ring — the launch line + the tagged thought + these three fill it exactly.
	for i := 0; i < 3; i++ {
		p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash"})
	}

	var taggedOlder string
	for _, line := range strings.Split(p.Frame(), "\n") {
		if strings.HasPrefix(line, thoughtPrefix+"coder"+subagentTagSeparator) {
			taggedOlder = line
		}
	}
	if taggedOlder == "" {
		t.Fatalf("tagged older line not found in frame: %q", p.Frame())
	}
	// The tag survived to the HEAD of the older, truncated line.
	if !strings.HasPrefix(taggedOlder, thoughtPrefix+"coder"+subagentTagSeparator) {
		t.Fatalf("tag did not survive at the head of the older line: %q", taggedOlder)
	}
	// And the body tail was cut: the line ends with the truncation ellipsis.
	if !strings.HasSuffix(taggedOlder, "…") {
		t.Fatalf("older tagged line should be tail-truncated with an ellipsis: %q", taggedOlder)
	}
	// The older line is capped to olderSnippetMax text runes (prefix rides on top), so the
	// tag genuinely traded line budget against the body tail rather than expanding the cap.
	prefixRunes := utf8.RuneCountInString(thoughtPrefix)
	if n := utf8.RuneCountInString(taggedOlder) - prefixRunes; n != olderSnippetMax {
		t.Fatalf("older tagged line text = %d runes, want olderSnippetMax %d", n, olderSnippetMax)
	}
}

// TestActivityLineTagMarkdownSanitized (AC3) pins the markdown-sanitization of the tag
// directly on the PER-LINE attribution path (not just transitively via the stage line):
// a subagent label full of markdown metacharacters from stageLabelMeta must be stripped
// before it is prepended to the body, so the tag can never inject inline markup into the
// rich-markdown frame. This is the one NEW injection surface the per-line tag adds.
func TestActivityLineTagMarkdownSanitized(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	// A label peppered with markdown metacharacters around readable content. Stripping
	// stageLabelMeta ("*_~`|=[]<>") from "a*b_c~d`|" leaves "abcd".
	rawLabel := "a*b_c~d`|"
	p.Observe(agentEvent(rawLabel, "toolu_2"))
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash", ParentID: "toolu_2"})

	var tagged string
	for _, line := range strings.Split(p.Frame(), "\n") {
		if strings.Contains(line, subagentTagSeparator) {
			tagged = line
		}
	}
	if tagged == "" {
		t.Fatalf("no tagged activity line found: %q", p.Frame())
	}
	// The tag carries the SANITIZED label (what sanitizeStageLabel actually produces for
	// this input) ahead of the separator.
	wantClean := sanitizeStageLabel(rawLabel)
	if wantClean != "abcd" {
		t.Fatalf("precondition: sanitizeStageLabel(%q) = %q, want %q", rawLabel, wantClean, "abcd")
	}
	if !strings.Contains(tagged, wantClean+subagentTagSeparator) {
		t.Fatalf("sanitized label %q not at the head of the tag: %q", wantClean, tagged)
	}
	// None of the stripped metacharacters survive into the rendered line.
	for _, meta := range strings.Split(stageLabelMeta, "") {
		if strings.Contains(tagged, meta) {
			t.Fatalf("stripped metacharacter %q leaked into the tagged line: %q", meta, tagged)
		}
	}
}

// TestActivityLineTaskLaunchAttribution (AC3) locks the Task launch path to the same
// per-line attribution contract as the Agent path: production decode stamps a ToolID on
// both. A Task launch carrying a ToolID, followed by an inner Bash whose ParentID matches,
// must tag that inner line with the subagent label.
func TestActivityLineTaskLaunchAttribution(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(taskEvent("coder", "toolu_t"))
	p.Observe(agent.Event{Type: agent.ToolUse, Tool: "Bash", ParentID: "toolu_t"})

	frame := p.Frame()
	wantLine := "⌨️ coder" + subagentTagSeparator + "Bash"
	if !strings.Contains(frame, wantLine) {
		t.Fatalf("Task-launch attributed line %q not in frame %q", wantLine, frame)
	}
}

// TestStageDistinctTrailFirstSeenOrder (AC2) asserts the trail lists each subagent
// once in first-seen order, with the most recent (active) one marked — and that a
// recurring subagent re-activates its EXISTING entry rather than appending a dupe.
func TestStageDistinctTrailFirstSeenOrder(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	for _, sub := range []string{"planner", "coder", "tester", "coder"} {
		p.Observe(taskEvent(sub))
	}
	// Distinct + first-seen order: planner, coder, tester (coder not duplicated).
	if got, want := strings.Join(p.stages, ","), "planner,coder,tester"; got != want {
		t.Fatalf("distinct-seen trail = %q, want %q", got, want)
	}
	// The last Task re-activated coder (index 1), not tester.
	if p.activeStage != 1 {
		t.Fatalf("active stage = %d, want coder at index 1", p.activeStage)
	}
	frame := p.Frame()
	wantLine := stagePrefix + "planner" + stageSeparator +
		stageActiveLeft + "coder" + stageActiveRight + stageSeparator + "tester"
	if !strings.Contains(frame, wantLine) {
		t.Fatalf("stage line %q not in frame %q", wantLine, frame)
	}
}

// TestStageLabelFallbackChain (AC2) asserts the label fallback chain when
// subagent_type is empty: subagent_type → description → "subagent". It never blanks
// and never panics on malformed input.
func TestStageLabelFallbackChain(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "subagent_type wins", input: `{"subagent_type":"reviewer","description":"d"}`, want: "reviewer"},
		{name: "falls back to description", input: `{"description":"build the thing"}`, want: "build the thing"},
		{name: "empty subagent_type falls to description", input: `{"subagent_type":"","description":"do it"}`, want: "do it"},
		{name: "both empty falls to constant", input: `{"subagent_type":"","description":""}`, want: stageLabelFallbck},
		{name: "missing fields fall to constant", input: `{"other":"x"}`, want: stageLabelFallbck},
		{name: "malformed json falls to constant", input: `{not json`, want: stageLabelFallbck},
		{name: "empty input falls to constant", input: ``, want: stageLabelFallbck},
		{name: "non-string field falls to constant", input: `{"subagent_type":123}`, want: stageLabelFallbck},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subagentLabel([]byte(tc.input)); got != tc.want {
				t.Fatalf("subagentLabel(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestStageLineCountedAgainstBudget asserts the stage header rides within
// frameBudgetMax even alongside a full ring of maximal lines, so the stage line can
// never push a frame past the Telegram cap.
func TestStageLineCountedAgainstBudget(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	p.Observe(taskEvent("coder"))
	for i := 0; i < 5; i++ {
		p.Observe(agent.Event{Type: agent.Text, Text: strings.Repeat("x", recentSnippetMax+500)})
	}
	frame := p.Frame()
	if n := utf8.RuneCountInString(frame); n > frameBudgetMax {
		t.Fatalf("frame with stage line exceeded budget: %d runes (max %d)", n, frameBudgetMax)
	}
	if !strings.Contains(frame, stagePrefix) {
		t.Fatalf("stage line dropped under budget pressure: %q", frame)
	}
}

// TestStageLineBoundedByManyDistinctStages asserts the stage header is bounded BY
// CONSTRUCTION: even when the stream produces MANY distinct Task subagent_types with
// long labels — free, model-controlled text, not the 5 known agents — alongside a
// full ring of maximal lines and a long elapsed, the assembled stage line stays
// within stageLineMax and the whole frame within frameBudgetMax. This is the case the
// single-stage TestStageLineCountedAgainstBudget does not exercise: there the header
// is tiny and only the ring-drop path is stressed; here the header itself is the risk.
func TestStageLineBoundedByManyDistinctStages(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")

	// A long elapsed (drives the widest "Working… (Nh Nm Ns)" header) plus 200 DISTINCT
	// long-labelled Task launches — each label is itself near the per-label cap.
	elapsed = 99*time.Hour + 59*time.Minute + 59*time.Second
	const distinctStages = 200
	for i := 0; i < distinctStages; i++ {
		// The distinguishing index leads so the labels stay DISTINCT after the
		// per-label stageLabelMax cap (advanceStage dedups by string equality); the
		// trailing run of 's' makes each one long, near the per-label cap.
		p.Observe(taskEvent(strconv.Itoa(i) + "-" + strings.Repeat("s", stageLabelMax)))
	}
	if len(p.stages) != distinctStages {
		t.Fatalf("expected %d distinct stages, got %d", distinctStages, len(p.stages))
	}
	// A full ring of maximal lines on top, so header + ring are both at their worst.
	for i := 0; i < 5; i++ {
		p.Observe(agent.Event{Type: agent.Text, Text: strings.Repeat("x", recentSnippetMax+500)})
	}

	// The stage line alone must be within its construction cap.
	stage := p.stageLine()
	if n := utf8.RuneCountInString(stage); n > stageLineMax {
		t.Fatalf("stage line exceeded stageLineMax: %d runes (max %d): %q", n, stageLineMax, stage)
	}
	// A trail this long must be elided (first … window), not listed in full.
	if !strings.Contains(stage, stageElision) {
		t.Fatalf("long stage trail not elided: %q", stage)
	}
	// The active (most recent) stage is the last launch and must still be marked.
	if !strings.Contains(stage, stageActiveLeft) || !strings.Contains(stage, stageActiveRight) {
		t.Fatalf("active stage not marked in elided trail: %q", stage)
	}

	// And the whole frame — header (with the bounded stage line) + full ring — stays
	// within the budget, the spec's hard requirement.
	frame := p.Frame()
	if n := utf8.RuneCountInString(frame); n > frameBudgetMax {
		t.Fatalf("frame with many-stage header exceeded budget: %d runes (max %d)", n, frameBudgetMax)
	}
}

// longLabel builds a DISTINCT label of exactly stageLabelMax runes (an index prefix
// plus an 's' run), mirroring the worst-case free-text labels the stream can produce.
func longLabel(i int) string {
	prefix := strconv.Itoa(i) + "-"
	return prefix + strings.Repeat("s", stageLabelMax-utf8.RuneCountInString(prefix))
}

// TestStageLineActiveKeptWithLongLabels is the regression for the short-trail shear:
// with a few stages whose labels sit at the per-label cap (stageLabelMax) the full
// first-seen join exceeds stageLineMax, so the OLD head-keeping truncate sheared the
// ACTIVE label (it sits at the tail) clean off. The unified atom-first assembly must
// instead keep the active stage present AND fully marked at every n in {3,4,5}, while
// the line stays within stageLineMax and emits no unbalanced markdown.
func TestStageLineActiveKeptWithLongLabels(t *testing.T) {
	for _, n := range []int{3, 4, 5} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			var elapsed time.Duration
			p := NewProgress(fakeClock(&elapsed), 5, "")
			for i := 0; i < n; i++ {
				p.Observe(taskEvent(longLabel(i)))
			}
			line := p.stageLine()

			// The line is bounded by construction.
			if got := utf8.RuneCountInString(line); got > stageLineMax {
				t.Fatalf("n=%d: stage line %d runes exceeds stageLineMax %d: %q", n, got, stageLineMax, line)
			}
			// These labels are long enough that the full join cannot fit — so this case
			// MUST exercise the elided assembly, not the whole-trail fast path.
			if !strings.Contains(line, stageElision) {
				t.Fatalf("n=%d: expected the long-label trail to be elided: %q", n, line)
			}
			// The invariant: the active stage is present AND marked as an atomic unit
			// (the regression was it being sheared off entirely).
			active := stageActiveLeft + sanitizeStageLabel(p.stages[p.activeStage]) + stageActiveRight
			if !strings.Contains(line, active) {
				t.Fatalf("n=%d: active stage not present-and-marked: want %q in %q", n, active, line)
			}
			// Markdown safety: the active markers are plain-text arrows and labels are
			// stripped of metacharacters, so a rich-markdown emphasis run can never be
			// left unbalanced. Assert no stray emphasis delimiters survive at all.
			assertMarkdownSafe(t, line)
		})
	}
}

// TestStageLineMarkdownSafeLabelContent asserts a label whose FREE-TEXT content carries
// markdown metacharacters (subagent_type/description are model-controlled) can never
// inject inline markup into the stage line: the metacharacters are stripped, the active
// stage stays marked with the plain-text arrows, and the line is markdown-balanced.
func TestStageLineMarkdownSafeLabelContent(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	// A label peppered with every delimiter the rich path treats specially, around the
	// readable word so sanitizing strips ONLY the metacharacters, not the content.
	p.Observe(taskEvent("*_~`|=[]<>reviewer*_~`|=[]<>"))
	line := p.stageLine()

	assertMarkdownSafe(t, line)
	// The sanitized label still renders and is marked as the active stage.
	active := stageActiveLeft + sanitizeStageLabel(p.stages[p.activeStage]) + stageActiveRight
	if !strings.Contains(line, active) {
		t.Fatalf("sanitized active label not marked: want %q in %q", active, line)
	}
	// Sanitization strips every markdown metacharacter, leaving the readable content.
	if got := sanitizeStageLabel(p.stages[0]); got != "reviewer" {
		t.Fatalf("metacharacters not stripped from label: %q", got)
	}
	// A label that is ALL metacharacters degrades to the fallback, never an empty marker.
	if got := sanitizeStageLabel("*_~`|=[]<>"); got != stageLabelFallbck {
		t.Fatalf("all-metacharacter label should fall back to %q, got %q", stageLabelFallbck, got)
	}
}

// TestStageLineActiveMarkedWhenRecurringInLongTrail covers a recurring active stage that
// is NOT at the tail of a long trail (a subagent relaunched after later ones): the window
// is anchored on the ACTIVE stage, so its marked unit is always shown even when it sits
// far from the most-recent end and the trail is elided on both sides.
func TestStageLineActiveMarkedWhenRecurringInLongTrail(t *testing.T) {
	var elapsed time.Duration
	p := NewProgress(fakeClock(&elapsed), 5, "")
	const distinctStages = 50
	for i := 0; i < distinctStages; i++ {
		p.Observe(taskEvent(longLabel(i)))
	}
	// Re-launch the FIRST subagent: it becomes active again at index 0, far from the tail.
	p.Observe(taskEvent(longLabel(0)))
	if p.activeStage != 0 {
		t.Fatalf("expected the recurring first stage to be active at index 0, got %d", p.activeStage)
	}

	line := p.stageLine()
	if got := utf8.RuneCountInString(line); got > stageLineMax {
		t.Fatalf("stage line %d runes exceeds stageLineMax %d: %q", got, stageLineMax, line)
	}
	active := stageActiveLeft + sanitizeStageLabel(p.stages[0]) + stageActiveRight
	if !strings.Contains(line, active) {
		t.Fatalf("recurring active (non-tail) stage not present-and-marked: want %q in %q", active, line)
	}
	assertMarkdownSafe(t, line)
}

// assertMarkdownSafe checks a rendered stage line carries no UNBALANCED inline-markup
// delimiter. The active markers are plain-text arrows (not "*"-emphasis) and label
// content is stripped of metacharacters, so every such delimiter count must be even
// (zero is the expected case here, but an even count is the real invariant the rich
// markdown parser needs to stay in sync).
func assertMarkdownSafe(t *testing.T, line string) {
	t.Helper()
	for _, meta := range []string{"*", "_", "`", "~", "|"} {
		if c := strings.Count(line, meta); c%2 != 0 {
			t.Fatalf("unbalanced %q in stage line (%d occurrences): %q", meta, c, line)
		}
	}
}

func TestFinalSuccess(t *testing.T) {
	out := Final(&agent.RunResult{Text: "  the answer is 42  ", IsError: false}, RunLimits{})
	if out != "the answer is 42" {
		t.Fatalf("got %q", out)
	}
}

func TestFinalErrorResult(t *testing.T) {
	out := Final(&agent.RunResult{Text: "max turns exceeded", IsError: true}, RunLimits{})
	if !strings.HasPrefix(out, "⚠️") || !strings.Contains(out, "max turns exceeded") {
		t.Fatalf("error result not flagged: %q", out)
	}

	empty := Final(&agent.RunResult{Text: "", IsError: true}, RunLimits{})
	if !strings.HasPrefix(empty, "⚠️") {
		t.Fatalf("empty error result not flagged: %q", empty)
	}

	// Diagnostic: the raw subtype survives into the message so the cause isn't
	// fully opaque, including for the subtype that has dedicated wording
	// (TestFinalTurnLimitStop covers what that wording says).
	withSub := Final(&agent.RunResult{Text: "", IsError: true, Subtype: turnLimitSubtype}, RunLimits{})
	if !strings.Contains(withSub, turnLimitSubtype) {
		t.Fatalf("empty error result should include the subtype: %q", withSub)
	}
	// With no subtype the plain fallback is kept (no empty parentheses).
	if empty != "⚠️ the run ended with an error" {
		t.Fatalf("empty error result without subtype should use the plain fallback: %q", empty)
	}

	if got := Final(&agent.RunResult{Text: "", IsError: false}, RunLimits{}); got != "(empty response)" {
		t.Fatalf("empty success placeholder: %q", got)
	}
	if got := Final(nil, RunLimits{}); got != "(no result)" {
		t.Fatalf("nil result: %q", got)
	}
}

// testMaxTurnsEnv stands in for the deployment's real turn-cap variable. core/chat
// must never hardcode a provider env name, so the tests do not name one either.
const testMaxTurnsEnv = "X_MAX_TURNS"

// TestFinalTurnLimitStop covers the terminal message for a run that exhausted the
// agent turn cap. It must state the cause, rule out a crash/timeout/API error,
// keep the raw subtype token, and name the knob plus the value actually in force —
// each clause appearing exactly when the limit behind it is known.
func TestFinalTurnLimitStop(t *testing.T) {
	tests := []struct {
		name    string
		res     *agent.RunResult
		limits  RunLimits
		want    []string
		notWant []string
	}{
		{
			name:   "cap and knob known",
			res:    &agent.RunResult{IsError: true, Subtype: turnLimitSubtype, NumTurns: 7},
			limits: RunLimits{MaxTurns: 40, MaxTurnsEnv: testMaxTurnsEnv},
			want: []string{
				"turn limit", "not a crash, timeout or API error",
				"(7 of 40 turns)", "`" + testMaxTurnsEnv + "`", "current: 40", turnLimitSubtype,
			},
		},
		{
			name:    "cap known but provider has no knob",
			res:     &agent.RunResult{IsError: true, Subtype: turnLimitSubtype, NumTurns: 7},
			limits:  RunLimits{MaxTurns: 40},
			want:    []string{"turn limit", "not a crash, timeout or API error", "(7 of 40 turns)", turnLimitSubtype},
			notWant: []string{testMaxTurnsEnv, "current:"},
		},
		{
			// No cap was passed to the provider, so its built-in limit applied: never
			// print a cap of 0, and say the variable is unset instead.
			name:    "knob known but no cap configured",
			res:     &agent.RunResult{IsError: true, Subtype: turnLimitSubtype, NumTurns: 7},
			limits:  RunLimits{MaxTurnsEnv: testMaxTurnsEnv},
			want:    []string{"turn limit", "built-in", "`" + testMaxTurnsEnv + "`", turnLimitSubtype},
			notWant: []string{"0", "current:", " of "},
		},
		{
			name:    "neither cap nor knob known",
			res:     &agent.RunResult{IsError: true, Subtype: turnLimitSubtype, NumTurns: 7},
			limits:  RunLimits{},
			want:    []string{"turn limit", "not a crash, timeout or API error", turnLimitSubtype},
			notWant: []string{testMaxTurnsEnv, "current:", " of ", "0"},
		},
		{
			// num_turns is the provider's own counter and may exceed the cap; a
			// "501 of 250" clause would read as a bug, so it is dropped entirely.
			name:    "reported turns exceed the cap",
			res:     &agent.RunResult{IsError: true, Subtype: turnLimitSubtype, NumTurns: 501},
			limits:  RunLimits{MaxTurns: 250, MaxTurnsEnv: testMaxTurnsEnv},
			want:    []string{"turn limit", "current: 250", turnLimitSubtype},
			notWant: []string{"501", " of "},
		},
		{
			name:    "no turn count reported",
			res:     &agent.RunResult{IsError: true, Subtype: turnLimitSubtype},
			limits:  RunLimits{MaxTurns: 40, MaxTurnsEnv: testMaxTurnsEnv},
			want:    []string{"turn limit", "current: 40", turnLimitSubtype},
			notWant: []string{" of "},
		},
		{
			// Text that came with the result used to be swallowed by the generic
			// wording; it is now kept and the explanation follows it.
			name:   "result text is kept and explained",
			res:    &agent.RunResult{Text: "partial answer", IsError: true, Subtype: turnLimitSubtype, NumTurns: 7},
			limits: RunLimits{MaxTurns: 40, MaxTurnsEnv: testMaxTurnsEnv},
			want:   []string{"⚠️ partial answer\n\nThe run stopped", "current: 40", turnLimitSubtype},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Final(tt.res, tt.limits)
			if !strings.HasPrefix(got, "⚠️") {
				t.Fatalf("turn-limit stop not flagged: %q", got)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("Final() = %q, want it to contain %q", got, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("Final() = %q, want it NOT to contain %q", got, notWant)
				}
			}
			assertMarkdownSafe(t, got)
		})
	}
}

// TestFinalKeepsWordingOutsideTurnLimit pins the exact terminal text for every
// result the turn-limit wording does not own, so threading RunLimits through
// Final cannot silently reword an unrelated message. The limits must make no
// difference to any of them.
func TestFinalKeepsWordingOutsideTurnLimit(t *testing.T) {
	tests := []struct {
		name string
		res  *agent.RunResult
		want string
	}{
		{"success text is trimmed", &agent.RunResult{Text: "  the answer is 42  "}, "the answer is 42"},
		{"empty success", &agent.RunResult{}, "(empty response)"},
		{"nil result", nil, "(no result)"},
		{"error with text", &agent.RunResult{Text: "boom", IsError: true}, "⚠️ boom"},
		{"error without subtype", &agent.RunResult{IsError: true}, "⚠️ the run ended with an error"},
		{
			// Another subtype's cause cannot be stated truthfully, so it keeps the
			// generic message with the raw token appended.
			"other error subtype",
			&agent.RunResult{IsError: true, Subtype: "error_during_execution"},
			"⚠️ the run ended with an error (error_during_execution)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Final(tt.res, RunLimits{}); got != tt.want {
				t.Fatalf("Final() with zero limits = %q, want %q", got, tt.want)
			}
			limits := RunLimits{MaxTurns: 40, MaxTurnsEnv: testMaxTurnsEnv}
			if got := Final(tt.res, limits); got != tt.want {
				t.Fatalf("Final() with limits = %q, want %q", got, tt.want)
			}
		})
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
