// Package verify is the deterministic post-run quality gate: after a run
// finishes cleanly, the bot re-runs the affected repos' OWN check runner
// (task/make/npm — the same entrypoint CI uses) from the Go side and compares
// reality against the agent's claim. The agent reporting "tests pass" counts
// for nothing here — this package re-executes the gate itself, so a run that
// left a repo red can be bounced back for a fix instead of being reported to
// the user as done.
//
// Scope discipline: only repos the RUN actually changed are verified. The
// service snapshots each repo's state (HEAD + porcelain status) before the run
// and hands the snapshot back after it; repos whose state is unchanged (a Q&A
// message, a read-only investigation) are skipped, so idle chatter never pays a
// test-suite run.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// gitCmdTimeout bounds the tiny git state commands (rev-parse/status) so a hung
// git (e.g. a dead network FS) can't stall the post-run path.
const gitCmdTimeout = 15 * time.Second

// defaultCheckTimeout bounds one repo's check command when the Verifier is
// constructed without an explicit timeout.
const defaultCheckTimeout = 30 * time.Minute

// termGrace is how long a timed-out gate command gets between SIGTERM and
// SIGKILL — enough for test runners to tear down containers and temp state.
const termGrace = 2 * time.Second

// outputTailBytes is how much trailing combined output is kept per check — the
// end of a failing run (the actual failures + summary) is what the fix-up
// prompt and the user notice need.
const outputTailBytes = 3500

// targetCandidates is the runner-target preference order. The first target the
// repo's runner actually defines is the one executed — `check`-style targets
// typically bundle lint+tests, matching the template's "gate" definition.
var targetCandidates = []string{"check", "ci", "test", "tests", "lint"}

// Verifier re-runs changed repos' check gates. A nil *Verifier disables the
// feature (both methods are safe no-ops on nil).
type Verifier struct {
	// Timeout bounds ONE repo's check command; non-positive falls back to
	// defaultCheckTimeout.
	Timeout time.Duration
	Log     *slog.Logger
}

// Snapshot maps a repo directory name to its pre-run state fingerprint.
type Snapshot map[string]string

// Result is the outcome of verifying one changed repo. Ran=false means no
// runner/target was detected (or the runner binary is absent) — that is a skip,
// not a failure. Command is the exact gate command for the user notice; Output
// is the trailing combined output (populated on failure).
type Result struct {
	Repo    string
	Command string
	Output  string
	Ran     bool
	Passed  bool
}

// log returns the configured logger or the default one.
func (v *Verifier) log() *slog.Logger {
	if v != nil && v.Log != nil {
		return v.Log
	}
	return slog.Default()
}

// timeout returns the per-check timeout, defaulted.
func (v *Verifier) timeout() time.Duration {
	if v == nil || v.Timeout <= 0 {
		return defaultCheckTimeout
	}
	return v.Timeout
}

// Take fingerprints every git repo directly under workspace dir. A nil
// Verifier, a missing git binary, or an unreadable dir yields an empty snapshot
// — Verify then sees no "changed" repos and does nothing, so a broken
// environment degrades to the old behavior instead of failing runs.
func (v *Verifier) Take(ctx context.Context, dir string) Snapshot {
	if v == nil {
		return nil
	}
	snap := Snapshot{}
	for _, repo := range discoverRepos(dir) {
		snap[repo] = repoFingerprint(ctx, filepath.Join(dir, repo))
	}
	return snap
}

// Verify re-runs the check gate of every repo whose fingerprint differs from
// the pre-run snapshot (including repos cloned DURING the run), plus every repo
// named in retry — the previous round's failures, which must be re-gated even
// when the fix run touched nothing (otherwise a no-op "fix" would silently
// exit the loop with the gate still red). It returns one Result per checked
// repo; an empty slice means nothing needed checking. A nil Verifier returns
// nil.
func (v *Verifier) Verify(ctx context.Context, dir string, before Snapshot, retry []string) []Result {
	if v == nil {
		return nil
	}
	retrySet := map[string]struct{}{}
	for _, r := range retry {
		retrySet[r] = struct{}{}
	}
	var results []Result
	for _, repo := range discoverRepos(dir) {
		path := filepath.Join(dir, repo)
		_, mustRetry := retrySet[repo]
		if !mustRetry && repoFingerprint(ctx, path) == before[repo] {
			continue
		}
		results = append(results, v.checkRepo(ctx, path, repo))
	}
	return results
}

// Failed filters results down to genuine gate failures (Ran && !Passed).
func Failed(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if r.Ran && !r.Passed {
			out = append(out, r)
		}
	}
	return out
}

// fixRoundLine renders (and fixRoundRe parses back) the loop-round marker
// embedded in every fix-up prompt. Carrying the round INSIDE the prompt makes
// the consecutive-round cap durable for free: the prompt is persisted in the
// auto-resume marker, so a deploy mid-loop resumes at the same round instead
// of resetting an in-memory counter to zero.
const fixRoundLine = "Auto-fix round %d of %d."

// fixRoundRe matches the marker at the start of a line, anchored so ordinary
// prose cannot fake it accidentally.
var fixRoundRe = regexp.MustCompile(`(?m)^Auto-fix round (\d+) of \d+\.$`)

// FixPrompt builds the prompt injected back into the working session when one
// or more gates failed. round is the 1-based fix-up round this prompt starts,
// out of maxRounds. It is an engineering artifact (professional English) and
// carries each failing command plus its output tail.
func FixPrompt(failed []Result, round, maxRounds int) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, fixRoundLine+"\n\n", round, maxRounds)
	_, _ = b.WriteString("Post-run verification re-ran the repos' own check gates and they FAILED. " +
		"The previous report of success does not hold.\n")
	for _, r := range failed {
		_, _ = fmt.Fprintf(&b, "\nRepo %s — `%s` exited non-zero. Output tail:\n```\n%s\n```\n",
			r.Repo, r.Command, r.Output)
	}
	_, _ = b.WriteString("\nFix the failures (never weaken or skip tests to get green), " +
		"re-run the same gate to confirm, and push the fix to the same branch if one was pushed.")
	return b.String()
}

// ParseFixRound extracts the round marker from a run's prompt: the fix-up
// round that run represents, or ok=false for a prompt that is not a
// verification fix-up (a user message, a poller relay, …).
func ParseFixRound(prompt string) (round int, ok bool) {
	m := fixRoundRe.FindStringSubmatch(prompt)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// checkRepo detects the repo's runner + target and executes the gate.
func (v *Verifier) checkRepo(ctx context.Context, path, repo string) Result {
	name, args, ok := detectGate(ctx, path)
	if !ok {
		v.log().Info("post-verify: no check runner detected; skipping", "repo", repo)
		return Result{Repo: repo, Ran: false}
	}
	cmdline := strings.Join(append([]string{name}, args...), " ")
	v.log().Info("post-verify: running gate", "repo", repo, "cmd", cmdline)

	out, err := runCmd(ctx, v.timeout(), path, name, args...)
	res := Result{Repo: repo, Command: cmdline, Ran: true, Passed: err == nil, Output: tail(out)}
	if err != nil {
		v.log().Warn("post-verify: gate failed", "repo", repo, "cmd", cmdline, "error", err)
	}
	return res
}

// detectGate picks the repo's check command: the first targetCandidates entry
// its runner defines, probing Taskfile → Makefile → package.json in that order.
// A runner file whose binary is missing falls through to the next runner.
func detectGate(ctx context.Context, path string) (name string, args []string, ok bool) {
	if hasFile(path, "Taskfile.yml") || hasFile(path, "Taskfile.yaml") {
		if _, err := exec.LookPath("task"); err == nil {
			for _, t := range targetCandidates {
				// `task --dry` resolves the task without executing; unknown → non-zero.
				if _, err := runCmd(ctx, gitCmdTimeout, path, "task", "--dry", t); err == nil {
					return "task", []string{t}, true
				}
			}
		}
	}
	if hasFile(path, "Makefile") {
		if _, err := exec.LookPath("make"); err == nil {
			for _, t := range targetCandidates {
				// `make -n` resolves the target without executing; no rule → non-zero.
				if _, err := runCmd(ctx, gitCmdTimeout, path, "make", "-n", t); err == nil {
					return "make", []string{t}, true
				}
			}
		}
	}
	if scripts := packageScripts(filepath.Join(path, "package.json")); len(scripts) > 0 {
		if _, err := exec.LookPath("npm"); err == nil {
			for _, t := range targetCandidates {
				if _, defined := scripts[t]; defined {
					return "npm", []string{"run", t}, true
				}
			}
		}
	}
	return "", nil, false
}

// hasFile reports whether dir contains the named regular file.
func hasFile(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}

// packageScripts parses package.json's "scripts" map; any failure yields nil.
func packageScripts(path string) map[string]string {
	data, err := os.ReadFile(path) //nolint:gosec // path is a join of internal workspace dirs.
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	return pkg.Scripts
}

// discoverRepos lists the git repos directly under dir (entries containing a
// .git directory or file), sorted by ReadDir's lexical order for determinism.
func discoverRepos(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), ".git")); err == nil {
			repos = append(repos, e.Name())
		}
	}
	return repos
}

// repoFingerprint captures a repo's HEAD plus its porcelain status. Any git
// failure yields a fingerprint that still compares stably ("" or partial), so a
// repo git cannot read is simply never reported as changed.
func repoFingerprint(ctx context.Context, path string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}
	head, _ := runCmd(ctx, gitCmdTimeout, path, "git", "rev-parse", "HEAD")
	status, _ := runCmd(ctx, gitCmdTimeout, path, "git", "status", "--porcelain")
	return strings.TrimSpace(head) + "\n" + status
}

// runCmd executes name+args in dir with the process in its own group, killing
// the whole group when ctx is cancelled or timeout elapses (check runners spawn
// tool subprocesses). It returns the combined output and the wait error (nil ==
// exit 0).
func runCmd(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// exec.Command (not CommandContext) is deliberate, mirroring core/claude:
	// cancellation must kill the whole process GROUP (check runners spawn tool
	// subprocesses), which the select below does explicitly.
	//nolint:gosec,noctx // name/args come from the fixed runner tables above; ctx drives the group kill below.
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-ctx.Done():
		// Mirror core/claude's kill strategy: SIGTERM first so the runner (and the
		// tool subprocesses it spawned — docker, builders) can tear down temp
		// state, escalating to SIGKILL only after a short grace.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(termGrace):
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			<-done
		}
		return buf.String(), ctx.Err()
	}
}

// tail returns the trailing outputTailBytes of s, trimmed.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= outputTailBytes {
		return s
	}
	return s[len(s)-outputTailBytes:]
}
