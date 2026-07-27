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
	// ansible marks a file whose tasks can carry a `when:`. A pull gated by one is the
	// opt-in update path (bot_image_pull) and is allowed. A compose file has no such
	// gate — a directive there fires on every `up`, so none is allowed.
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
	regexp.MustCompile(`docker(?:[\s,"']+compose|-compose)?[\s,"']+pull\b`),
	regexp.MustCompile(`\b(?:community\.docker\.)?docker_image(?:_pull)?\s*:`),
}

var (
	yamlListItem = regexp.MustCompile(`^(\s*)-\s`)
	yamlWhenKey  = regexp.MustCompile(`^\s*when:`)
)

// TestDeployNeverPullsImplicitly asserts that no shipped compose file and no Ansible task
// fetches an image on its own. Updating the bot is the operator's decision — a run with
// `-e bot_image_pull=true`, or an explicit pull on the host — never a side effect of a
// playbook run made for something else, like a token rotation or a limit change. An
// UNCONDITIONAL pull fails; a pull that its own task's `when:` gates is that opt-in and
// passes, so the guard catches the return of the implicit behaviour, not the update path.
func TestDeployNeverPullsImplicitly(t *testing.T) {
	for _, m := range deployManifests {
		raw, err := os.ReadFile(m.path)
		if err != nil {
			// Keep going: one unreadable manifest must not hide a pull in the others.
			t.Errorf("read %s: %v", m.path, err)
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			// Comments may name a directive to explain why it is absent.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, re := range pullDirectives {
				if !re.MatchString(line) {
					continue
				}
				if m.ansible && taskHasWhen(lines, i) {
					continue // the opt-in update path
				}
				t.Errorf("%s:%d matches %q: %s\ndeploys must not pull implicitly — moving to a new "+
					"image is an explicit operator action, so a pull has to sit in a task gated by "+
					"its own `when:` (see bot_image_pull)", m.path, i+1, re, strings.TrimSpace(line))
			}
		}
	}
}

// taskHasWhen reports whether the Ansible task holding line i carries its own `when:`. The
// task spans from the list item opening it to the next list item at the same or shallower
// indentation. Deliberately strict: a condition inherited from an enclosing `block:` does
// not count, because someone reading the task cannot see that it is gated.
func taskHasWhen(lines []string, i int) bool {
	start := i
	for start >= 0 && !yamlListItem.MatchString(lines[start]) {
		start--
	}
	if start < 0 {
		return false
	}
	indent := len(yamlListItem.FindStringSubmatch(lines[start])[1])
	for j := start + 1; j < len(lines); j++ {
		if m := yamlListItem.FindStringSubmatch(lines[j]); m != nil && len(m[1]) <= indent {
			return false // next task started
		}
		if yamlWhenKey.MatchString(lines[j]) {
			return true
		}
	}
	return false
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
		`    cmd: docker compose pull`,
		`    cmd: docker-compose pull bot`,
		`    cmd: docker pull ghcr.io/duckbugio/flock-telegram:latest`,
		`    argv: ["docker", "compose", "pull"]`,
		`  community.docker.docker_image:`,
		`  community.docker.docker_image_pull:`,
		`  docker_image:`,
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
		`    pull_policy: missing`,
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

// TestBotImagePullIsOptIn asserts the knob the pull task is gated on defaults to false.
// A default of true would make that `when:` always fire, which is an unconditional pull
// wearing a gate — exactly the behaviour the guard above exists to keep out.
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
	if got := strings.Trim(m[1], `"'`); got != "false" {
		t.Errorf("%s: bot_image_pull defaults to %q, want \"false\" — updating must stay opt-in, "+
			"otherwise every playbook run pulls again", path, got)
	}
}
