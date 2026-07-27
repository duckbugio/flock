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

// Files that decide whether a playbook run may replace the running image.
var deployManifests = []string{
	"docker-compose.yml",
	"deploy/roles/claude_tg_bot/templates/docker-compose.yml.j2",
	"deploy/roles/claude_tg_bot/tasks/main.yml",
}

// Directives that make `docker compose up` re-resolve a moving tag (":latest") against
// the registry, so an unrelated playbook run silently ships whatever landed on main
// since the last deploy. Both spellings are matched loosely (quoting, "--pull=always")
// because they are the same instruction re-spelled.
var implicitPullDirectives = []*regexp.Regexp{
	regexp.MustCompile(`--pull[ =]+["']?always`),
	regexp.MustCompile(`pull_policy:\s*["']?always`),
}

// TestDeployNeverPullsImplicitly asserts no shipped compose file or Ansible task re-pulls
// the image on its own. Updating the bot is the operator's decision (an explicit
// `docker compose pull`), never a side effect of a playbook run made for something else
// — a token rotation or a limit change must not also roll the version forward.
func TestDeployNeverPullsImplicitly(t *testing.T) {
	for _, name := range deployManifests {
		raw, err := os.ReadFile(name) //nolint:gosec // G304: fixed repo-relative path, no input
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Comments may name the directive to explain why it is absent.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, re := range implicitPullDirectives {
				if re.MatchString(line) {
					t.Errorf("%s:%d matches %q: %s\ndeploys must not pull implicitly — "+
						"moving to a new image is an explicit operator action", name, i+1, re, strings.TrimSpace(line))
				}
			}
		}
	}
}
