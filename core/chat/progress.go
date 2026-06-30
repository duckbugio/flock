package chat

// This file holds the transport-agnostic progress rendering: turning a claude
// event stream plus wall-clock ticks into the text of a live "Working… (Ns)"
// progress message. It knows nothing about any chat platform's API so it can be
// unit-tested in isolation. Final-answer chunking lives in chunk.go.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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

// toolDetailSeparator joins the tool name and its extracted detail: "📖 Read · path".
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

// separatorRunes is the rune count of the "\n\n" blank-line separator placed
// between each block of a frame — the header, the optional "+N earlier" indicator,
// and every activity line — so the steps read as visually separated blocks.
const separatorRunes = 2

// Activity-line prefixes: a thought balloon for the model's text, and a wrench as
// the fallback for a tool call that has no more specific emoji (see toolPrefixes).
const (
	thoughtPrefix = "💭 "
	toolPrefix    = "🔧 "
)

// toolPrefixes maps a lowercased tool name to the emoji prefix shown before its
// activity line, so the live frame reads "📖 Read · …" / "⌨️ Bash · …" instead of a
// generic wrench for everything. A tool not listed here falls back to toolPrefix.
// Several tools deliberately share an emoji (the edit family, the search family)
// because they are the same kind of action from the user's point of view.
var toolPrefixes = map[string]string{
	"read":         "📖 ",
	"edit":         "✏️ ",
	"write":        "✏️ ",
	"notebookedit": "✏️ ",
	"bash":         "⌨️ ",
	"grep":         "🔍 ",
	"glob":         "🔍 ",
	"task":         "🤖 ",
	"agent":        "🤖 ",
	"webfetch":     "🌐 ",
	"websearch":    "🔎 ",
	"toolsearch":   "🔎 ",
	"skill":        "🧩 ",
}

// toolLinePrefix returns the emoji prefix for a tool's activity line, falling back
// to the generic wrench for an unknown or empty tool name.
func toolLinePrefix(tool string) string {
	if p, ok := toolPrefixes[strings.ToLower(tool)]; ok {
		return p
	}
	return toolPrefix
}

// activityPrefixes is every emoji prefix an activity line can begin with — the
// thought balloon, the fallback wrench, and each per-tool emoji. capLine/Frame use
// it to peel the prefix off before applying the rune budget. It is built once from
// the renderer's own constants so it can never drift from what activityLine emits.
// No prefix is a byte-prefix of another (each is a distinct emoji + space), so the
// scan order does not matter.
var activityPrefixes = buildActivityPrefixes()

func buildActivityPrefixes() []string {
	prefixes := []string{thoughtPrefix, toolPrefix}
	seen := map[string]bool{thoughtPrefix: true, toolPrefix: true}
	for _, p := range toolPrefixes {
		if !seen[p] {
			seen[p] = true
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

// elidedFormat renders the "+N earlier" indicator prepended to the activity block
// when older lines have scrolled off above the visible window. The leading clock
// emoji matches the activity-line style (every line carries an emoji prefix), so
// the indicator reads as part of the ring instead of a bare stray line.
const elidedFormat = "🕓 +%d earlier"

// Stage-line markup. The header above the activity ring shows which dev-team
// subagents (planner/coder/tester/…) the run has launched, in first-seen order,
// with the currently active one wrapped in stageActiveLeft/stageActiveRight so the
// user can tell which stage is running now. The whole line is prefixed with
// stagePrefix to match the emoji-led style of every other frame line, and the
// labels are joined by stageSeparator.
//
// The active markers are PLAIN-TEXT arrows, not markdown emphasis: the stage line
// ships inside a frame sent with asMarkdown=true (the Telegram Rich Markdown path,
// where a single "*" is italic), and the active marker must never depend on a pair
// of "*" staying balanced. A markdown-neutral "▸ … ◂" can never desync the parser
// no matter where the line is clamped, and — together with sanitizeStageLabel,
// which strips markdown metacharacters from the free-text label CONTENT — guarantees
// the rendered stage line is markdown-safe regardless of label content OR length.
const (
	stagePrefix       = "🧭 "
	stageSeparator    = " → "
	stageActiveLeft   = "▸ "
	stageActiveRight  = " ◂"
	stageLabelFallbck = "subagent"
)

// stageLabelMeta is the set of markdown metacharacters stripped from a subagent
// label's free-text CONTENT (subagent_type/description are model-controlled) before
// it is placed in the stage line. The rich path treats "*" "_" "~" "`" "|" "=" "["
// "]" as inline-markup delimiters; an odd one left in a label would desync the
// parser and break the whole frame. Stripping them (the label is purely cosmetic)
// keeps the line markdown-safe by CONSTRUCTION, complementing the plain-text active
// markers. "<" ">" are dropped too so a label can never look like an HTML tag on the
// legacy MarkdownToHTML path either.
const stageLabelMeta = "*_~`|=[]<>"

// stageLabelMax bounds a single subagent label in the stage line so an unexpected
// long subagent_type/description can't blow up the header; the whole stage line is
// itself hard-capped at stageLineMax (see stageLine) so the always-kept header can
// never push a frame past frameBudgetMax.
const stageLabelMax = 40

// stageLineMax hard-caps the WHOLE assembled stage line (prefix + elided trail) in
// runes. subagent_type/description are model/stream-controlled free text and the
// trail can in principle list arbitrarily many DISTINCT labels, so the line is
// bounded BY CONSTRUCTION here — the line is assembled ATOM-FIRST around the ACTIVE
// stage's marked unit (added first so it is guaranteed kept and fully marked), then
// older/newer neighbours are added only while the line stays under stageLineMax (the
// first stage's label followed by an ellipsis leads the trail when earlier stages were
// dropped) — so the final truncateRunes is a pure no-op safety net that can never
// shear off the active marker. The header the line rides in therefore stays bounded
// regardless of how many stages were observed. It is sized to comfortably hold the
// active label plus a few neighbours at stageLabelMax.
const stageLineMax = 120

// stageWindowMax is how many labels the elided trail aims to keep around the active
// stage, subject to stageLineMax. When more labels fall outside that window the trail
// leads with the first stage's label and an ellipsis so the user still sees where the
// run began and where it is now without an unbounded list. The active stage's marked
// unit is always the first label taken when assembling, so it is never the one dropped
// to make room.
const stageWindowMax = 4

// stageElision is the marker shown in place of the labels skipped between the first
// stage and the most-recent window when the trail is too long to render in full.
const stageElision = "…"

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
	// baseDir is the run's working directory. File-path tool details are shown
	// relative to it (see repoRelPath) so the progress frame reads
	// "📖 Read · roost/web/…" instead of the noisy absolute "/workspace/chat_…/…".
	// An empty baseDir disables relativization (paths are shown unchanged).
	baseDir string
	// total counts every activity line ever pushed (not no-op/ignored events), so
	// Frame can show how many lines have scrolled off above the visible window.
	total int
	// stages is the deterministic trail of DISTINCT subagent labels seen, in
	// first-seen order — pure membership taken from the stream, not an inferred
	// round count. activeStage indexes the currently running subagent within it
	// (the most recent Task/Agent launch). Both stay empty for a plain chat with no
	// subagent launches, so the stage line is omitted and the frame is byte-identical
	// to a non-pipeline run. State changes ONLY on a Tool=="Task"/"Agent" ToolUse (see
	// Observe); inner subagent tool events never flip the active stage.
	stages      []string
	activeStage int
	// subagentByToolID maps a launching Agent/Task tool_use id (Event.ToolID) to the
	// subagent label it spawned, so an inner event whose Event.ParentID matches can be
	// tagged with that label (see activityLine). It is populated only when a launch
	// carries a non-empty ToolID; it stays nil/empty when the live stream omits
	// tool_use ids or parent_tool_use_id, in which case per-line attribution is a
	// no-op with zero regression. Attribution assumes the launching tool_use envelope
	// is decoded BEFORE any inner envelope carrying its id as parent_tool_use_id (the
	// CLI guarantees this ordering); out-of-order arrival degrades to an untagged line
	// (a benign no-op).
	subagentByToolID map[string]string
}

// NewProgress returns a Progress whose header counter reads from elapsed, the
// time since the run started. Injecting elapsed (rather than calling time.Now
// internally) keeps Frame deterministic for tests. ringSize <= 0 selects the
// default. baseDir is the run's working directory used to shorten file-path tool
// details to repo-relative form (empty disables it; see repoRelPath).
func NewProgress(elapsed func() time.Duration, ringSize int, baseDir string) *Progress {
	if ringSize <= 0 {
		ringSize = defaultRingSize
	}
	return &Progress{
		elapsed:     elapsed,
		ringSize:    ringSize,
		baseDir:     baseDir,
		ring:        make([]string, 0, ringSize),
		activeStage: -1,
	}
}

// Observe folds a single stream event into the activity ring. Terminal events
// (Result, RunError) carry no activity line and are ignored here; callers use
// Final / FinalError to render the terminal message. It reports whether the
// activity ring changed, so callers can skip a redundant edit.
func (p *Progress) Observe(e claude.Event) bool {
	// A subagent launch (the Lead spawning a dev-team subagent — via the Task OR the
	// Agent tool, depending on the harness) advances the stage header. This is the
	// ONLY event that changes the active stage: an inner subagent's own tool events
	// arrive flat at the top level (linked back only by ParentID), so they are
	// ordinary ToolUse events with a non-task/agent tool name and must not flip the
	// active stage. When the launch carries a tool_use id, remember it so the
	// subagent's inner activity lines can be attributed to it (see activityLine).
	if e.Type == claude.ToolUse && isSubagentLaunch(e.Tool) {
		label := subagentLabel(e.ToolInput)
		p.advanceStage(label)
		if e.ToolID != "" {
			if p.subagentByToolID == nil {
				p.subagentByToolID = make(map[string]string)
			}
			p.subagentByToolID[e.ToolID] = label
		}
	}
	line, ok := activityLine(e, p.baseDir, p.subagentLabelFor(e.ParentID))
	if !ok {
		return false
	}
	p.push(line)
	return true
}

// isSubagentLaunch reports whether a tool name is a dev-team subagent launch. Both
// the Task tool and the Agent tool spawn a subagent (the harness picks one), so
// either advances the stage trail; every other tool is ordinary inner activity.
func isSubagentLaunch(tool string) bool {
	return strings.EqualFold(tool, "task") || strings.EqualFold(tool, "agent")
}

// subagentLabelFor returns the subagent label remembered for parentID (a launching
// Agent/Task tool_use id), or "" when parentID is empty or not a known launch. A
// returned label tags the event's activity line with the subagent that produced it.
func (p *Progress) subagentLabelFor(parentID string) string {
	if parentID == "" {
		return ""
	}
	return p.subagentByToolID[parentID]
}

// advanceStage records label as the currently active subagent, appending it to the
// distinct-seen trail the first time it appears (preserving first-seen order) and
// re-selecting it as active when it recurs. Membership only: it never counts rounds.
func (p *Progress) advanceStage(label string) {
	for i, s := range p.stages {
		if s == label {
			p.activeStage = i
			return
		}
	}
	p.stages = append(p.stages, label)
	p.activeStage = len(p.stages) - 1
}

// subagentLabel extracts the subagent label from a Task tool's JSON input, with the
// fallback chain subagent_type → description → "subagent". It never returns blank
// and never panics on malformed input (an unparseable body yields the fallback).
func subagentLabel(input json.RawMessage) string {
	var args map[string]any
	if len(input) > 0 {
		_ = json.Unmarshal(input, &args)
	}
	strField := func(k string) string {
		if v, ok := args[k].(string); ok {
			return collapseWhitespace(v)
		}
		return ""
	}
	if v := strField("subagent_type"); v != "" {
		return truncateRunes(v, stageLabelMax)
	}
	if v := strField("description"); v != "" {
		return truncateRunes(v, stageLabelMax)
	}
	return stageLabelFallbck
}

// sanitizeStageLabel strips markdown metacharacters (stageLabelMeta) from a stage
// label's free-text content so the cosmetic label can never inject inline markup
// into the stage line, which rides in a frame sent on the rich (markdown) path. It
// is applied at RENDER time (in stageLine), not at capture, so p.stages keeps the
// raw label for membership/dedup and only the displayed copy is sanitized. A label
// that becomes empty after stripping (all-metacharacter) falls back to the constant
// so the active stage is never rendered blank.
func sanitizeStageLabel(label string) string {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune(stageLabelMeta, r) {
			return -1
		}
		return r
	}, label)
	cleaned = collapseWhitespace(cleaned)
	if cleaned == "" {
		return stageLabelFallbck
	}
	return cleaned
}

// activityLine renders the one-line activity summary for an event, or false if
// the event contributes no visible activity. The TEXT is capped at a generous
// upper bound (recentSnippetMax) when stored — enough for any ring position — and
// Frame() re-caps it tighter per recency. The emoji prefix is added on top of the
// text budget, so it is never split or counted against the cap.
//
// subagentTag, when non-empty, is the dev-team subagent the event was produced
// inside (resolved from the event's ParentID); it is prepended to the line BODY,
// right after the emoji prefix ("⌨️ coder ▸ Bash"), so the tag survives the recency
// re-cap and the prefix-peeling in capLine. An empty tag (a top-level event, or
// a stream that omits parent linkage) renders EXACTLY as before — byte-identical.
func activityLine(e claude.Event, baseDir, subagentTag string) (string, bool) {
	switch e.Type {
	case claude.ToolUse:
		tool := e.Tool
		if tool == "" {
			tool = "tool"
		}
		line := tool
		// Bash is shown as just "⌨️ Bash": the command itself is long, noisy, and adds
		// little to the live progress (and can carry secrets). Every other tool still
		// shows its detail (file path, pattern, query…) — only Bash is bare.
		if !strings.EqualFold(tool, "bash") {
			if detail := toolDetail(tool, e.ToolInput, baseDir); detail != "" {
				line += toolDetailSeparator + detail
			}
		}
		return toolLinePrefix(tool) + withSubagentTag(subagentTag, truncateRunes(line, recentSnippetMax)), true
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
		return thoughtPrefix + withSubagentTag(subagentTag, truncateRunes(snippet, recentSnippetMax)), true
	default:
		// SystemInit, ToolResult, Result, RunError: no activity line.
		return "", false
	}
}

// subagentTagSeparator joins a subagent label to the activity it produced:
// "coder ▸ Bash · …". It is a plain-text marker (no markdown metacharacters) so it
// can never desync the rich-markdown parser the live frame is sent on.
const subagentTagSeparator = " ▸ "

// withSubagentTag prepends the subagent label (sanitized markdown-safe) to a line
// body so an inner subagent's activity reads "coder ▸ Bash · …". The tag goes at the
// HEAD of the body — after the caller's emoji prefix, before the tool/thought text —
// so it survives the tail-truncation in capLine/Frame and capLine's emoji-prefix
// peeling still works unchanged. An empty label is a no-op: the body is returned
// untouched, so a top-level event renders byte-identical to before this feature.
//
// The tag consumes line budget, so an attributed line's content is truncated
// ~tag-length earlier than the byte-identical untagged line — intentional, since the
// attribution is higher-signal than the trailing chars of a long command.
func withSubagentTag(label, body string) string {
	if label == "" {
		return body
	}
	clean := sanitizeStageLabel(label)
	return clean + subagentTagSeparator + body
}

// toolDetail extracts a short, human-readable detail from a tool's JSON input so a
// tool activity line can show WHAT is being read/run, not just the tool name. It is
// best-effort and purely cosmetic: any empty, malformed, or unrecognized input
// yields "" so the caller falls back to the bare tool name. The chosen value is
// whitespace-collapsed and truncated to toolDetailMax runes so one long command or
// path can't dominate the line. baseDir (the run's working dir) shortens file-path
// details to repo-relative form; see repoRelPath.
func toolDetail(tool string, input json.RawMessage, baseDir string) string {
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

	// For the file-path tools, shorten the absolute path to repo-relative BEFORE
	// whitespace-collapse and truncation, so the budget is spent on the meaningful
	// tail (repo/dir/file) rather than the constant "/workspace/chat_…" prefix.
	if key == "file_path" {
		raw = repoRelPath(raw, baseDir)
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

// repoRelPath shortens an absolute file path to one relative to baseDir (the run's
// working directory) for the cosmetic progress line, turning the noisy
// "/workspace/chat_123/roost/web/src/App.tsx" into "roost/web/src/App.tsx" — the
// repo segment is kept since baseDir is the workspace root, not the repo root. It
// is deliberately conservative and never fabricates "../.." noise: when the path
// can't be cleanly expressed under baseDir it returns the input unchanged. The
// fallbacks (all return path as-is): an empty baseDir (relativization disabled), a
// filepath.Rel error, or a result that escapes baseDir (starts with ".."). Paths in
// this codebase are POSIX, so filepath.Rel on linux already yields '/' separators.
func repoRelPath(path, baseDir string) string {
	if baseDir == "" {
		return path
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return path
	}
	// A leading ".." means the path is outside baseDir (e.g. a sibling dir or an
	// absolute path that doesn't share the prefix); keep the original full path
	// rather than show "../../something". A bare "." means the path IS baseDir (no
	// real file_path is the workspace root, but guard it so we never render "·  .").
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
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
	// keySepValue (below) also tolerates a closing quote between the key and its ':'
	// so a JSON request body in a shell command ('{"password":"x"}') is masked too.
	{
		regexp.MustCompile(`(?i)\b([\w.-]*` +
			`(?:token|secret|password|passwd|api[_-]?key|access[_-]?token)` +
			`[\w.-]*)` + keySepValue),
		"${1}${2}" + redactedMask,
	},
	// "auth" alone is too common in benign commands ("go test ./auth", "cd auth
	// && …") to mask on a bare space, so it ONLY redacts when bound to its value by
	// ':' / '=' ("--auth=token", "auth: x", '"auth":"x"') — not by whitespace. Like
	// the rule above it tolerates an identifier prefix so "X_AUTH=…" is still caught.
	{
		regexp.MustCompile(`(?i)\b([\w.-]*auth)(\s*["']?\s*[:=]\s*)` + valuePattern),
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

// valuePattern matches the VALUE half of a credential key/value pair. A bare \S+
// stops at the first space, which would leak the tail of a *quoted* multi-word
// value (`--password "hunter 2 spaces"`); the quote alternatives consume the
// whole quoted run so it is masked as a unit. Stays linear under RE2.
const valuePattern = `(?:"[^"]*"|'[^']*'|\S+)`

// keySepValue is the SEPARATOR + VALUE half of the strong-keyword credential rule.
// Group 2 (the separator) binds a keyword to its value either by ':' / '=' — with
// an optional closing quote between them so a JSON body in a shell command
// (`'{"password":"x"}'`) is masked, not just `--password=x` — or by bare
// whitespace (`--password hunter2`). The value itself (valuePattern) is consumed
// but not captured, so the replacement keeps ${1}${2} and masks only the value.
const keySepValue = `(\s*["']?\s*[:=]\s*|\s+)` + valuePattern

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
	for _, prefix := range activityPrefixes {
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

// stageLine renders the compact subagent-trail header shown above the activity
// ring: the distinct subagents launched so far, in first-seen order, joined by an
// arrow with the currently active one marked. It returns "" when no Task event has
// been observed, so a plain chat with zero Task events omits the line entirely (the
// frame stays byte-identical to a non-pipeline run).
//
// Assembly is ATOM-FIRST so the active stage can NEVER be sheared off, no matter how
// long the labels or how many distinct stages there were:
//   - When the full first-seen trail fits stageLineMax it is shown whole (the common
//     short case, byte-identical to listing every marked label joined by an arrow).
//   - Otherwise the line is elided: the active stage's MARKED UNIT is placed first as
//     an atomic unit (so it is always present and fully marked), neighbours are added
//     around it in first-seen order only while the line stays under stageLineMax, and
//     the first stage's label + an ellipsis (leading) / a trailing ellipsis mark the
//     gaps to the unshown ends.
//
// Because the line is already <= stageLineMax after assembly, the final truncateRunes
// is a pure no-op safety net that can never cut the active marker. Each label is itself
// bounded at stageLabelMax (subagentLabel) and stripped of markdown metacharacters
// (sanitizeStageLabel), and the active markers are plain-text arrows, so the header is
// markdown-safe and within frameBudgetMax regardless of input.
func (p *Progress) stageLine() string {
	if len(p.stages) == 0 {
		return ""
	}
	n := len(p.stages)
	mark := func(i int) string {
		label := sanitizeStageLabel(p.stages[i])
		if i == p.activeStage {
			return stageActiveLeft + label + stageActiveRight
		}
		return label
	}

	// Fast path: the WHOLE trail (every label, first-seen order) fits — show it in full.
	// This keeps the common short-trail case identical to a plain marked join.
	full := make([]string, n)
	for i := range p.stages {
		full[i] = mark(i)
	}
	whole := stagePrefix + strings.Join(full, stageSeparator)
	if utf8.RuneCountInString(whole) <= stageLineMax {
		return whole
	}

	// Elide. Grow a contiguous window [lo, hi] OUTWARD from the active stage, whose
	// marked unit is placed first and is therefore the one element never dropped to make
	// room. Recent context (toward the tail) is preferred, then older context, taking a
	// neighbour only while the rendered line — including the leading/trailing elision for
	// any unshown ends — stays within stageLineMax.
	lo, hi := p.activeStage, p.activeStage
	render := func(lo, hi int) string {
		parts := make([]string, 0, n)
		// Lead with the first stage when it is not already inside the window, plus an
		// elision only when there is a genuine gap (>1 index) between it and the window.
		if lo > 0 {
			parts = append(parts, mark(0))
			if lo > 1 {
				parts = append(parts, stageElision)
			}
		}
		for i := lo; i <= hi; i++ {
			parts = append(parts, mark(i))
		}
		if hi < n-1 {
			parts = append(parts, stageElision)
		}
		return stagePrefix + strings.Join(parts, stageSeparator)
	}
	fits := func(lo, hi int) bool {
		return utf8.RuneCountInString(render(lo, hi)) <= stageLineMax
	}
	// Defensive: the active marked unit alone (no neighbours, no elision) must fit; if a
	// pathologically long active label blows the cap even by itself, surface it alone and
	// let truncateRunes clamp it — the active stage is the single most important element.
	if !fits(lo, hi) {
		return truncateRunes(stagePrefix+mark(p.activeStage), stageLineMax)
	}
	// Expand the window, alternating tail-side then head-side, while it still fits and
	// while it has not reached the stageWindowMax span budget on either side.
	for hi-lo+1 < stageWindowMax {
		grewTail := hi < n-1 && fits(lo, hi+1)
		if grewTail {
			hi++
			if hi-lo+1 >= stageWindowMax {
				break
			}
		}
		grewHead := lo > 0 && fits(lo-1, hi)
		if grewHead {
			lo--
		}
		if !grewTail && !grewHead {
			break
		}
	}
	return truncateRunes(render(lo, hi), stageLineMax)
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
	// The optional stage header rides directly under "Working…", separated by a
	// blank line like every other block. It is empty (and so contributes nothing,
	// keeping a no-Task chat byte-identical) when no Task event has been observed.
	if stage := p.stageLine(); stage != "" {
		header += "\n\n" + stage
	}
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
		// Join the header, the optional "+N earlier" indicator and each activity line
		// with a blank line between them, so the steps read as separated blocks.
		blocks := []string{header}
		if hidden := p.total - len(ringLines); hidden > 0 {
			blocks = append(blocks, fmt.Sprintf(elidedFormat, hidden))
		}
		blocks = append(blocks, ringLines...)
		return strings.Join(blocks, "\n\n")
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
		// The indicator block plus its own blank-line separator before the line.
		overhead += separatorRunes + utf8.RuneCountInString(fmt.Sprintf(elidedFormat, hidden))
	}
	for _, prefix := range activityPrefixes {
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
