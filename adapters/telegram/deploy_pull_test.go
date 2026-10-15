package telegram_test

// Guard for a DEPLOY invariant, not for Go code: moving a deployment to a new image
// must stay an explicit operator action. It lives in this package because every file
// it guards ships under adapters/telegram/ (the shipped compose file plus the Ansible
// role), and `go test` runs with the package directory as its working directory, so
// the paths below are stable relative paths. There is no deploy-only Go package to
// host it, and `task tests` is the only gate CI runs — a Go test is what gets executed.

import (
	"io/fs"
	"os"
	"path"
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

// Every shipped file that can NAME a pull directive. It is not the whole surface that
// decides whether a deploy may replace the running image: the files that can flip the
// opt-in knob those directives are gated on are a moving set (group_vars, host_vars, a
// second inventory, a play's own `vars:`), so TestBotImagePullIsOptIn walks for them
// instead of listing them here.
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
	// The module whose arguments are text for the operator rather than a command to run.
	debugModule = regexp.MustCompile(`^(?:ansible\.builtin\.)?debug$`)
)

// pullFinding is one line that fetches an image without the opt-in gate.
type pullFinding struct {
	line int // 0-based index into the document's lines
	re   *regexp.Regexp
	text string
}

// TestDeployNeverPullsImplicitly asserts that no shipped compose file and no Ansible task
// asks for a newer image on its own. Moving a deployment onto another build is the
// operator's decision — a run with `-e bot_image_pull=true`, or a pull on the host — rather
// than a by-product of a run made for something else, like a token rotation or a limit
// change. What it cannot promise is that a run never reaches the registry: compose's default
// policy still fetches a tag this host has no image for (see defaults/main.yml).
//
// What it checks, exactly: a pull is accepted only when it sits in an Ansible task whose
// OWN `when:` (a key at the task's own indentation, not one inherited from an enclosing
// `block:`) names bot_image_pull, whose default TestBotImagePullIsOptIn pins to false — or
// in a debug task, which executes nothing and only prints a command for the operator.
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
			if ansible && pullIsAllowed(lines, i) {
				continue
			}
			found = append(found, pullFinding{line: i, re: re, text: strings.TrimSpace(line)})
		}
	}
	return found
}

// pullIsAllowed reports whether a directive on line i is one the invariant permits. Exactly
// two shapes are:
//   - the opt-in update path: the task DIRECTLY holding the line has its own `when:` naming
//     bot_image_pull;
//   - operator-facing text: the line belongs to a debug task, which executes nothing. Its
//     msg is the role TALKING about a command — "to update by hand, run …" — and a guard
//     that reddens CI over the sentence documenting the manual path is a guard people
//     delete. The whole task is skipped rather than only its msg, which is the readable
//     rule; the price is that a `vars:` on a debug task is unscanned too, so a command
//     smuggled through a `lookup('pipe', …)` there would pass. This catches accidents, not
//     evasion — as the guard's own docblock says, it is not an evaluator.
func pullIsAllowed(lines []string, i int) bool {
	keys := taskOwnKeys(lines, i)
	for key := range keys {
		if debugModule.MatchString(key) {
			return true
		}
	}
	return gateVariable.MatchString(keys["when"])
}

// taskOwnKeys returns the keys of the task that DIRECTLY contains line i, each mapped to
// its value with any continuation lines appended. "Directly" is what makes the answer
// independent of key order: the task is the nearest list item at or above line i, and only
// keys at that item's own indentation are its own — so a `when:` on an enclosing `block:` is
// not picked up whether it is written before or after the block, and the module arguments
// (or a nested block's tasks) below the key line cannot smuggle one in. A condition
// inherited from a block deliberately does not count: someone reading the task cannot see
// that it is gated, and the gate is the invariant. Returns nil when line i is not inside a
// list item at all, so a caller reading a key off the result gets the empty string.
func taskOwnKeys(lines []string, i int) map[string]string {
	start := i
	for start >= 0 && !startsMapping(lines[start]) {
		start--
	}
	if start < 0 {
		return nil
	}
	dash := len(yamlListItem.FindStringSubmatch(lines[start])[1])
	keyIndent, ok := taskKeyIndent(lines, start, dash)
	if !ok {
		return nil
	}

	keys := map[string]string{}
	current := ""
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
			// A block scalar, a list value, module arguments, or a nested block's tasks.
			appendKeyValue(keys, current, line)
			continue
		}
		key := yamlKey.FindStringSubmatch(line)
		if key == nil {
			appendKeyValue(keys, current, line) // a list value at key indentation
			continue
		}
		current = key[1]
		_, value, _ := strings.Cut(line, ":")
		appendKeyValue(keys, current, value)
	}
	return keys
}

// startsMapping reports whether a line opens a list item that is a MAPPING — "- key:", or a
// bare "-" whose keys follow below. A task is always one; a list item holding a scalar
// ("- to update by hand: run …", an entry of a debug msg list) is a VALUE, and the task it
// belongs to is further up. Walking past those is what lets a directive named inside a msg
// list be attributed to the debug task printing it.
func startsMapping(line string) bool {
	m := yamlListItem.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	rest := strings.TrimLeft(stripComment(line)[len(m[1])+1:], " \t")
	return rest == "" || yamlKey.MatchString(rest)
}

// appendKeyValue adds a line to the value collected for key, ignoring text that belongs to
// no key yet.
func appendKeyValue(keys map[string]string, key, value string) {
	if key == "" {
		return
	}
	keys[key] = strings.TrimSpace(keys[key] + " " + strings.TrimSpace(value))
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

// quoteOpener is where a quoted scalar can START: the beginning of the line, or after
// whitespace or one of the flow punctuators. Anywhere else — inside a word — a quote is the
// literal character YAML reads it as, which is what makes "the operator's call" prose and
// not an unterminated scalar.
const quoteOpener = " \t[{,:"

// stripComment removes a YAML end-of-line comment: a "#" that starts the line or follows
// whitespace, and is not inside a quoted scalar. Quote tracking keeps a "#" that is part of
// a value (e.g. a colour or a fragment URL in quotes) from truncating a real directive, and
// the opener rule keeps an apostrophe in a word from opening a scalar that never closes —
// which would leave the comment on the line and fail the build over a sentence saying the
// directive is NOT used. Both mistakes are worth avoiding, but not symmetrically: cutting
// too early hides a real directive, so the rule stays "a quote only quotes where a scalar
// can begin" rather than "stop tracking quotes when one is left open".
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case (c == '\'' || c == '"') && (i == 0 || strings.IndexByte(quoteOpener, line[i-1]) >= 0):
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
			name: "a debug task telling the operator how to update by hand",
			doc: `---
- name: Report status
  ansible.builtin.debug:
    msg:
      - "to update by hand: docker compose pull && docker compose up -d"
`,
			accepted: true, // debug runs nothing; this is the role TALKING about the command
		},
		{
			name: "the short module name is a debug task too",
			doc: `---
- name: Report status
  debug:
    msg: "update by hand with: docker compose pull"
`,
			accepted: true,
		},
		{
			name: "a command task is not excused by quoting the same instruction",
			doc: `---
- name: Report status
  ansible.builtin.command:
    cmd: docker compose pull && docker compose up -d
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
		// An apostrophe inside a word is the literal character YAML reads it as, not the
		// start of a quoted scalar — treating it as one leaves the quote open to the end of
		// the line, so the comment is never cut off and the guard fires on prose.
		`    msg: Updating is the operator's call   # not a docker compose pull`,
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
		// Cutting too early is the worse failure: it hides a real directive. A quoted "#"
		// still has to be skipped on a line that also contains an apostrophe, so the fix
		// for the case above cannot be "give up and cut at the first # after a space".
		`    cmd: echo "it's # here" && docker compose pull`,
	}
	for _, line := range visible {
		if !matchesPullDirective(stripComment(line)) {
			t.Errorf("a trailing comment hides a directive: %s", line)
		}
	}
}

const (
	// deployRoot is the shipped deploy tree: the role plus the example inventory.
	deployRoot = "deploy"
	// exampleInventory is the only inventory this repo ships; .gitignore keeps every other
	// inventories/<name>/ out, and an operator's local copy is not this test's business.
	exampleInventory = "example"
	// botImagePullDefault is where the knob has to be defined, so the gate has a value to
	// read when no inventory mentions it.
	botImagePullDefault = "deploy/roles/claude_tg_bot/defaults/main.yml"
)

// botImagePullSetting matches a shipped ASSIGNMENT of the opt-in knob, in either syntax
// Ansible reads one from: a YAML mapping key (role defaults, group_vars, host_vars, a
// play's `vars:`) or an INI entry in an inventory's [group:vars] section. Anchoring it at
// the start of the line is what keeps `-e bot_image_pull=true` inside a documented command
// line, and `when: bot_image_pull | bool` (whose key is `when`), from reading as one.
var botImagePullSetting = regexp.MustCompile(`^\s*bot_image_pull\s*[:=]\s*(\S+)`)

// varSetting is one assignment of a variable in a shipped file.
type varSetting struct {
	line  int // 0-based index into the document's lines
	value string
}

// TestBotImagePullIsOptIn asserts that nothing this repo ships turns the update switch on.
// A true value would make the pull task's `when:` always fire, which is an unconditional
// pull wearing a gate — exactly the behaviour the guard above exists to keep out. The guard
// checks that the gate NAMES this knob; this test is what makes naming it mean "off".
//
// It checks every shipped file, not just the role default, because a role default is the
// weakest source Ansible has: the example inventory's group_vars — the directory the README
// tells operators to copy — outranks it, so `bot_image_pull: true` there would put the
// implicit update back into every derived deployment while the default still reads false.
func TestBotImagePullIsOptIn(t *testing.T) {
	deploy := os.DirFS(deployRoot)
	sources, err := shippedVarSources(deploy)
	if err != nil {
		t.Fatalf("walk %s: %v", deployRoot, err)
	}
	defaulted := false
	for _, rel := range sources {
		name := path.Join(deployRoot, rel)
		raw, err := fs.ReadFile(deploy, rel)
		if err != nil {
			// Keep going: one unreadable file must not hide a setting in the others.
			t.Errorf("read %s: %v", name, err)
			continue
		}
		for _, set := range botImagePullSettings(string(raw)) {
			if name == botImagePullDefault {
				defaulted = true
			}
			if isFalseValue(set.value) {
				continue
			}
			t.Errorf("%s:%d sets bot_image_pull to %q, want a false value — updating must stay "+
				"opt-in, otherwise every playbook run fetches images again", name, set.line+1, set.value)
		}
	}
	if !defaulted {
		t.Errorf("%s: no bot_image_pull default — the pull task's `when:` gate must have one",
			botImagePullDefault)
	}
}

// shippedVarSources returns every tracked file in the deploy tree that Ansible could read a
// variable from. It walks instead of listing: group_vars, host_vars, a second inventory and
// a play's own `vars:` all outrank a role default, so a fixed list would be one forgotten
// file away from letting the switch back on. Inventories other than the example are skipped
// — .gitignore keeps them out of the repo, so they are the operator's, not ours.
func shippedVarSources(deploy fs.FS) ([]string, error) {
	var files []string
	err := fs.WalkDir(deploy, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, name)
			return nil
		}
		if path.Dir(name) == "inventories" && d.Name() != exampleInventory {
			return fs.SkipDir
		}
		return nil
	})
	return files, err
}

// botImagePullSettings returns every assignment of the opt-in knob in a shipped file.
// Comments come off first, so the update command quoted in the role's own documentation is
// not read as an assignment.
func botImagePullSettings(content string) []varSetting {
	var found []varSetting
	for i, raw := range strings.Split(content, "\n") {
		if m := botImagePullSetting.FindStringSubmatch(stripComment(raw)); m != nil {
			found = append(found, varSetting{line: i, value: m[1]})
		}
	}
	return found
}

// isFalseValue reports whether Ansible reads the value as false: the YAML 1.1 booleans in
// any case, plus 0, which `| bool` (how the task consumes the knob) also treats as false.
func isFalseValue(value string) bool {
	falsey := map[string]bool{"false": true, "no": true, "n": true, "off": true, "0": true}
	return falsey[strings.ToLower(strings.Trim(value, `"'`))]
}
