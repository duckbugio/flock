//nolint:testpackage // intentionally whitebox to test unexported verify internals
package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireTools skips the test when the binaries it drives are absent (the
// dev-tools image ships git and make; a bare host might not).
func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not available: %v", n, err)
		}
	}
}

// gitRepo initializes a real git repo with one committed file and returns the
// workspace dir + repo name.
func gitRepo(t *testing.T, makefile string) (ws, repo string) {
	t.Helper()
	ws = t.TempDir()
	repo = "app"
	dir := filepath.Join(ws, repo)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		//nolint:gosec,noctx // test helper drives git with fixed args; no ctx needed
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if makefile != "" {
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(makefile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", ".")
	run("-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	return ws, repo
}

func TestUnchangedRepoIsSkipped(t *testing.T) {
	requireTools(t, "git")
	v := &Verifier{}
	ws, _ := gitRepo(t, "check:\n\ttrue\n")
	ctx := context.Background()
	snap := v.Take(ctx, ws)
	if results := v.Verify(ctx, ws, snap, nil); len(results) != 0 {
		t.Fatalf("unchanged repo must produce no results, got %+v", results)
	}
}

func TestChangedRepoGatePasses(t *testing.T) {
	requireTools(t, "git", "make")
	v := &Verifier{Timeout: time.Minute}
	ws, repo := gitRepo(t, "check:\n\t@echo gate ok\n")
	ctx := context.Background()
	snap := v.Take(ctx, ws)
	// Dirty the working tree — the fingerprint changes.
	if err := os.WriteFile(filepath.Join(ws, repo, "main.txt"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := v.Verify(ctx, ws, snap, nil)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %+v", results)
	}
	r := results[0]
	if !r.Ran || !r.Passed || r.Command != "make check" {
		t.Fatalf("result = %+v", r)
	}
	if len(Failed(results)) != 0 {
		t.Fatal("passing gate must not be reported failed")
	}
}

func TestChangedRepoGateFails(t *testing.T) {
	requireTools(t, "git", "make")
	v := &Verifier{Timeout: time.Minute}
	ws, repo := gitRepo(t, "check:\n\t@echo boom failure detail; exit 1\n")
	ctx := context.Background()
	snap := v.Take(ctx, ws)
	if err := os.WriteFile(filepath.Join(ws, repo, "extra.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := v.Verify(ctx, ws, snap, nil)
	failed := Failed(results)
	if len(failed) != 1 {
		t.Fatalf("want 1 failed, got %+v", results)
	}
	if !strings.Contains(failed[0].Output, "boom failure detail") {
		t.Fatalf("failure output not captured: %+v", failed[0])
	}
	prompt := FixPrompt(failed, 1, 2)
	if !strings.Contains(prompt, "make check") || !strings.Contains(prompt, "boom failure detail") {
		t.Fatalf("FixPrompt must carry command and output:\n%s", prompt)
	}
	// The loop round rides inside the prompt (that is what makes the cap durable
	// across restarts) and parses back exactly.
	if round, ok := ParseFixRound(prompt); !ok || round != 1 {
		t.Fatalf("ParseFixRound(FixPrompt) = %d, %v; want 1, true", round, ok)
	}
	if _, ok := ParseFixRound("an ordinary user message about Auto-fix round things"); ok {
		t.Fatal("prose must not parse as a fix-round marker")
	}
}

func TestRepoWithoutRunnerIsSkipResult(t *testing.T) {
	requireTools(t, "git")
	v := &Verifier{}
	ws, repo := gitRepo(t, "")
	ctx := context.Background()
	snap := v.Take(ctx, ws)
	if err := os.WriteFile(filepath.Join(ws, repo, "extra.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := v.Verify(ctx, ws, snap, nil)
	if len(results) != 1 || results[0].Ran {
		t.Fatalf("runnerless repo must yield a skip result, got %+v", results)
	}
	if len(Failed(results)) != 0 {
		t.Fatal("a skip is not a failure")
	}
}

func TestNewRepoClonedDuringRunIsVerified(t *testing.T) {
	requireTools(t, "git", "make")
	v := &Verifier{Timeout: time.Minute}
	ws := t.TempDir()
	ctx := context.Background()
	snap := v.Take(ctx, ws) // empty workspace
	// A repo appears mid-run (as if cloned by the agent).
	ws2, repo := gitRepo(t, "check:\n\t@echo ok\n")
	if err := os.Rename(filepath.Join(ws2, repo), filepath.Join(ws, repo)); err != nil {
		t.Fatal(err)
	}
	results := v.Verify(ctx, ws, snap, nil)
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("new repo must be verified, got %+v", results)
	}
}

func TestRetryRepoIsRecheckedEvenWhenUnchanged(t *testing.T) {
	requireTools(t, "git", "make")
	v := &Verifier{Timeout: time.Minute}
	ws, repo := gitRepo(t, "check:\n\t@exit 1\n")
	ctx := context.Background()
	snap := v.Take(ctx, ws)
	// Nothing changed since the snapshot, but the repo is on the retry list (its
	// gate failed last round) — it must be re-gated anyway.
	results := v.Verify(ctx, ws, snap, []string{repo})
	if len(Failed(results)) != 1 {
		t.Fatalf("retry repo must be re-gated, got %+v", results)
	}
	// An unknown retry name matches no checkout and is ignored.
	if res := v.Verify(ctx, ws, snap, []string{"ghost"}); len(res) != 0 {
		t.Fatalf("unknown retry repo must be ignored, got %+v", res)
	}
}

func TestNilVerifierIsNoop(t *testing.T) {
	var v *Verifier
	ctx := context.Background()
	if snap := v.Take(ctx, t.TempDir()); snap != nil {
		t.Fatal("nil Take must return nil")
	}
	if res := v.Verify(ctx, t.TempDir(), nil, nil); res != nil {
		t.Fatal("nil Verify must return nil")
	}
}

func TestPackageScriptsAndPick(t *testing.T) {
	dir := t.TempDir()
	pkg := `{"name":"x","scripts":{"lint":"eslint .","test":"jest"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o600); err != nil {
		t.Fatal(err)
	}
	scripts := packageScripts(filepath.Join(dir, "package.json"))
	if len(scripts) != 2 || scripts["test"] != "jest" {
		t.Fatalf("scripts = %+v", scripts)
	}
	if s := packageScripts(filepath.Join(dir, "absent.json")); s != nil {
		t.Fatal("missing file must yield nil")
	}
}
