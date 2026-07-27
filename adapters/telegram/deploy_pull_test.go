package telegram_test

// Guard for a DEPLOY invariant, not for Go code: moving a deployment to a new image
// must stay an explicit operator action. It lives in this package because every file
// it guards ships under adapters/telegram/ (the shipped compose file plus the Ansible
// role), and `go test` runs with the package directory as its working directory, so
// the paths below are stable relative paths. There is no deploy-only Go package to
// host it, and `task tests` is the only gate CI runs — a Go test is what gets executed.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// deployManifest is a shipped file that can make a deploy fetch an image.
type deployManifest struct {
	path string
	// ansible marks a file whose tasks can carry a `when:`. A pull whose OWN task is
	// gated on bot_image_pull is the opt-in update path and is allowed. A compose file
	// has no such gate — a directive there fires on every `up`, so none is allowed.
	ansible bool
}

// Every file that decides whether a deploy may replace the running image.
var deployManifests = []deployManifest{
	{path: "docker-compose.yml"},
	{path: "deploy/roles/claude_tg_bot/templates/docker-compose.yml.j2"},
	{path: "deploy/playbook.yml", ansible: true},
	{path: "deploy/roles/claude_tg_bot/tasks/main.yml", ansible: true},
	{path: "deploy/roles/claude_tg_bot/handlers/main.yml", ansible: true},
}

// The spellings that fetch an image during a deploy: compose's pull flag and pull policy
// (both re-resolve a moving tag on every `up`), an outright `docker [compose] pull`, and
// the Ansible modules that pull one. Matched loosely on purpose — quoting, `--pull=always`
// and the argv form ["docker", "compose", "up", "-d", "--pull", "always"] are the same
// instruction re-spelled. Matching is per line, which is how every one of these is written
// in practice; a directive split across several YAML lines is out of reach.
var pullDirectives = []*regexp.Regexp{
	regexp.MustCompile(`--pull["']?[\s,=]*["']?always`),
	regexp.MustCompile(`pull_policy:\s*["']?always`),
	// The compose module's own fetch option. `\b` keeps this off `bot_image_pull:`, where
	// the preceding `_` is a word character and so leaves no boundary.
	regexp.MustCompile(`\bpull:\s*["']?always`),
	regexp.MustCompile(`docker(?:[\s,"']+compose|-compose)?[\s,"']+pull\b`),
	regexp.MustCompile(`\b(?:community\.docker\.)?docker_image(?:_pull)?\s*:`),
	// The compose modules themselves: whether they fetch depends on their `pull:` option
	// and on the compose file's own policy, so a switch to one has to be reviewed here
	// (and gated, or this guard widened) rather than slipped in.
	regexp.MustCompile(`\b(?:community\.docker\.)?docker_compose(?:_v2)?\s*:`),
}

var (
	yamlListItem = regexp.MustCompile(`^(\s*)-(?:\s|$)`)
	yamlKey      = regexp.MustCompile(`^\s*([\w.-]+):(?:\s|$)`)
	// The opt-in knob a pull's own condition has to name (see defaults/main.yml).
	gateVariable = regexp.MustCompile(`\bbot_image_pull\b`)
)

// pullFinding is one line that fetches an image without the opt-in gate.
type pullFinding struct {
	line int // 0-based index into the document's lines
	re   *regexp.Regexp
	text string
}

// TestDeployNeverPullsImplicitly asserts that no shipped compose file and no Ansible task
// fetches an image on its own. Updating is the operator's decision — a run with
// `-e bot_image_pull=true`, or an explicit pull on the host — never a side effect of a
// playbook run made for something else, like a token rotation or a limit change.
//
// What it checks, exactly: a pull is accepted only when it sits in an Ansible task whose
// OWN `when:` (a key at the task's own indentation, not one inherited from an enclosing
// `block:`) names bot_image_pull, whose default TestBotImagePullIsOptIn pins to false.
// What it does NOT check: that the condition is semantically equivalent to that knob —
// `when: bot_image_pull | bool or true` names it and would pass. It catches an ungated
// pull, and a pull gated on the wrong variable or on a constant; it is not an evaluator.
func TestDeployNeverPullsImplicitly(t *testing.T) {
	for _, m := range deployManifests {
		raw, err := os.ReadFile(m.path)
		if err != nil {
			// Keep going: one unreadable manifest must not hide a pull in the others.
			t.Errorf("read %s: %v", m.path, err)
			continue
		}
		for _, f := range findUngatedPulls(string(raw), m.ansible) {
			t.Errorf("%s:%d matches %q: %s\ndeploys must not pull implicitly — moving to a new "+
				"image is an explicit operator action, so a pull has to sit in a task whose own "+
				"`when:` gates it on bot_image_pull", m.path, f.line+1, f.re, f.text)
		}
	}
}

// findUngatedPulls returns every line of a deploy manifest that fetches an image without
// the opt-in gate. ansible enables the gate: in a compose file nothing can gate a pull.
func findUngatedPulls(content string, ansible bool) []pullFinding {
	lines := strings.Split(content, "\n")
	var found []pullFinding
	for i, raw := range lines {
		// A comment may name a directive to explain why it is absent — whole-line or
		// trailing — so drop comments before matching instead of only skipping lines
		// that are entirely one.
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, re := range pullDirectives {
			if !re.MatchString(line) {
				continue
			}
			if ansible && pullIsGated(lines, i) {
				continue // the opt-in update path
			}
			found = append(found, pullFinding{line: i, re: re, text: strings.TrimSpace(line)})
		}
	}
	return found
}

// pullIsGated reports whether the Ansible task holding line i is gated on the opt-in knob.
func pullIsGated(lines []string, i int) bool {
	cond, ok := taskOwnCondition(lines, i)
	return ok && gateVariable.MatchString(cond)
}

// taskOwnCondition returns the `when:` expression of the task that DIRECTLY contains line
// i. "Directly" is what makes the answer independent of key order: the task is the nearest
// list item at or above line i, and only keys at that item's own indentation are its own —
// so a `when:` on an enclosing `block:` is not picked up whether it is written before or
// after the block, and the module arguments (or a nested block's tasks) below the key line
// cannot smuggle one in. A condition inherited from a block deliberately does not count:
// someone reading the task cannot see that it is gated, and the gate is the invariant.
func taskOwnCondition(lines []string, i int) (string, bool) {
	start := i
	for start >= 0 && !yamlListItem.MatchString(lines[start]) {
		start--
	}
	if start < 0 {
		return "", false
	}
	dash := len(yamlListItem.FindStringSubmatch(lines[start])[1])
	keyIndent, ok := taskKeyIndent(lines, start, dash)
	if !ok {
		return "", false
	}

	var cond []string
	inWhen := false
	for j := start; j < len(lines); j++ {
		line := stripComment(lines[j])
		if j == start {
			// Blank out the "-" so the item's first key is read like any other key line.
			line = strings.Repeat(" ", dash+1) + line[dash+1:]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if m := yamlListItem.FindStringSubmatch(line); m != nil && len(m[1]) <= dash {
			break // a sibling task (or a shallower list) starts
		}
		if indent < keyIndent {
			break // back out to the enclosing mapping: this task ended
		}
		if indent > keyIndent {
			if inWhen {
				cond = append(cond, strings.TrimSpace(line)) // a block scalar or a list value
			}
			continue // module arguments, or a nested block's tasks
		}
		key := yamlKey.FindStringSubmatch(line)
		if key == nil {
			if inWhen {
				cond = append(cond, strings.TrimSpace(line)) // a list value at key indentation
			}
			continue
		}
		if key[1] == "when" {
			inWhen = true
			_, value, _ := strings.Cut(line, ":")
			cond = append(cond, strings.TrimSpace(value))
			continue
		}
		inWhen = false
	}
	if len(cond) == 0 {
		return "", false
	}
	return strings.Join(cond, " "), true
}

// taskKeyIndent returns the column at which the keys of the list item on line start begin.
func taskKeyIndent(lines []string, start, dash int) (int, bool) {
	rest := lines[start][dash+1:]
	if trimmed := strings.TrimLeft(rest, " \t"); trimmed != "" {
		return dash + 1 + len(rest) - len(trimmed), true
	}
	// A bare "-": the item's keys start on the next non-blank line.
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		indent := len(lines[j]) - len(strings.TrimLeft(lines[j], " \t"))
		return indent, indent > dash
	}
	return 0, false
}

// stripComment removes a YAML end-of-line comment: a "#" that starts the line or follows
// whitespace, and is not inside a quoted scalar. Quote tracking keeps a "#" that is part
// of a value (e.g. a colour or a fragment URL in quotes) from truncating a real directive.
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// TestPullDirectivesMatchKnownSpellings pins what the guard above promises to catch, so a
// pattern narrowed by a later edit fails here instead of silently letting a pull back in.
func TestPullDirectivesMatchKnownSpellings(t *testing.T) {
	pulls := []string{
		`    cmd: docker compose up -d --pull always --force-recreate`,
		`    cmd: docker compose up -d --pull=always`,
		`    argv: ["docker", "compose", "up", "-d", "--pull", "always"]`,
		`    pull_policy: always`,
		`    pull_policy: "always"`,
		`    pull: always`,
		`    pull: "always"`,
		`    cmd: docker compose pull`,
		`    cmd: docker-compose pull bot`,
		`    cmd: docker pull ghcr.io/duckbugio/flock-telegram:latest`,
		`    argv: ["docker", "compose", "pull"]`,
		`  community.docker.docker_image:`,
		`  community.docker.docker_image_pull:`,
		`  docker_image:`,
		`  community.docker.docker_compose_v2:`,
		`  docker_compose:`,
	}
	for _, line := range pulls {
		if !matchesPullDirective(line) {
			t.Errorf("no pattern matches a pull: %s", line)
		}
	}

	// Lines the role legitimately ships; a pattern that matches these would make the
	// guard fire on unrelated edits and get deleted.
	clean := []string{
		`    cmd: docker compose up -d --force-recreate`,
		`    cmd: docker compose restart`,
		`    image: {{ bot_image }}`,
		`bot_image_pull: false`,
		`  when: bot_image_pull | bool`,
		`    pull_policy: missing`,
		`    pull: never`,
	}
	for _, line := range clean {
		if matchesPullDirective(line) {
			t.Errorf("pattern fires on a line that pulls nothing: %s", line)
		}
	}
}

func matchesPullDirective(line string) bool {
	for _, re := range pullDirectives {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// TestPullGateAcceptsOnlyTheOptInTask pins the gate itself: which spellings of a gated
// pull the guard accepts, and — the part that carries the invariant — which near-misses
// it still reports. Every case holds exactly one `docker compose pull`, so "no finding"
// means "accepted as the opt-in path".
func TestPullGateAcceptsOnlyTheOptInTask(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		accepted bool
	}{
		{
			name: "the role's own opt-in task",
			doc: `---
- name: Fetch the images (opt-in update)
  ansible.builtin.command:
    cmd: docker compose pull
    chdir: "{{ app_dir }}"
  when: bot_image_pull | bool
  changed_when: false
`,
			accepted: true,
		},
		{
			name: "when: written above the module keys",
			doc: `---
- name: Fetch the images (opt-in update)
  when: bot_image_pull | bool
  ansible.builtin.command:
    cmd: docker compose pull
`,
			accepted: true, // key order in a YAML mapping carries no meaning
		},
		{
			name: "when: as a list",
			doc: `---
- name: Fetch the images (opt-in update)
  ansible.builtin.command:
    cmd: docker compose pull
  when:
    - bot_image_pull | bool
    - app_dir | length > 0
`,
			accepted: true,
		},
		{
			name: "gated by an enclosing block, when: after it",
			doc: `---
- name: Update
  block:
    - name: Fetch the images
      ansible.builtin.command:
        cmd: docker compose pull
  when: bot_image_pull | bool
`,
			accepted: false, // the gate has to be visible on the task that pulls
		},
		{
			name: "gated by an enclosing block, when: before it",
			doc: `---
- name: Update
  when: bot_image_pull | bool
  block:
    - name: Fetch the images
      ansible.builtin.command:
        cmd: docker compose pull
`,
			accepted: false, // same verdict as above: the answer cannot depend on order
		},
		{
			name: "gate on a constant",
			doc: `---
- name: Fetch the images
  ansible.builtin.command:
    cmd: docker compose pull
  when: true
`,
			accepted: false,
		},
		{
			name: "gate on an unrelated variable",
			doc: `---
- name: Fetch the images
  ansible.builtin.command:
    cmd: docker compose pull
  when: enable_dind | bool
`,
			accepted: false,
		},
		{
			name: "the knob named only in a comment",
			doc: `---
- name: Fetch the images
  ansible.builtin.command:
    cmd: docker compose pull
  when: enable_dind | bool   # bot_image_pull
`,
			accepted: false,
		},
		{
			name: "no condition at all",
			doc: `---
- name: Fetch the images
  ansible.builtin.command:
    cmd: docker compose pull
`,
			accepted: false,
		},
		{
			name: "the next task's gate does not reach back",
			doc: `---
- name: Fetch the images
  ansible.builtin.command:
    cmd: docker compose pull

- name: Something else
  ansible.builtin.command:
    cmd: docker compose up -d
  when: bot_image_pull | bool
`,
			accepted: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := findUngatedPulls(tc.doc, true)
			if tc.accepted && len(found) > 0 {
				t.Errorf("gated pull reported as implicit: line %d, %s", found[0].line+1, found[0].text)
			}
			if !tc.accepted && len(found) == 0 {
				t.Error("ungated pull accepted as the opt-in path")
			}
		})
	}
}

// TestStripCommentKeepsDirectivesVisible pins the comment handling the guard relies on: a
// commented-out directive must not fail the build, and a comment appended to a real line
// must not hide the directive on it.
func TestStripCommentKeepsDirectivesVisible(t *testing.T) {
	hidden := []string{
		`# No --pull always here: updating is opt-in`,
		`    # cmd: docker compose pull`,
	}
	for _, line := range hidden {
		if matchesPullDirective(stripComment(line)) {
			t.Errorf("a comment naming a directive fails the guard: %s", line)
		}
	}

	visible := []string{
		`    cmd: docker compose pull   # opt-in update`,
		`    pull_policy: always  # keep us current`,
		`    cmd: echo "nothing # here" && docker compose pull`,
	}
	for _, line := range visible {
		if !matchesPullDirective(stripComment(line)) {
			t.Errorf("a trailing comment hides a directive: %s", line)
		}
	}
}

// TestBotImagePullIsOptIn asserts the knob the pull task is gated on defaults to false.
// A default of true would make that `when:` always fire, which is an unconditional pull
// wearing a gate — exactly the behaviour the guard above exists to keep out. The guard
// checks that the gate NAMES this knob; this test is what makes naming it mean "off".
func TestBotImagePullIsOptIn(t *testing.T) {
	const path = "deploy/roles/claude_tg_bot/defaults/main.yml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`(?m)^bot_image_pull:\s*(\S+)`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("%s: no bot_image_pull default — the pull task's `when:` gate must have one", path)
	}
	// Every spelling Ansible reads as false: the YAML 1.1 booleans in any case, plus 0,
	// which `| bool` (how the task consumes it) also treats as false.
	falsey := map[string]bool{"false": true, "no": true, "n": true, "off": true, "0": true}
	if got := strings.Trim(m[1], `"'`); !falsey[strings.ToLower(got)] {
		t.Errorf("%s: bot_image_pull defaults to %q, want a false value — updating must stay "+
			"opt-in, otherwise every playbook run pulls again", path, got)
	}
}
