//nolint:testpackage // intentionally whitebox to test unexported gitsetup helpers
package gitsetup

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// gitConfigGet reads a global git config value with HOME pointed at dir.
func gitConfigGet(t *testing.T, home, key string) (string, bool) {
	t.Helper()
	//nolint:gosec,noctx // G204/noctx: test invokes git/sh with controlled args; no ctx needed in a test helper
	cmd := exec.Command("git", "config", "--global", "--get", key)
	cmd.Env = append([]string{"HOME=" + home}, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 means the key is absent.
		return "", false
	}
	return strings.TrimRight(string(out), "\n"), true
}

func TestApplyIdentityAndDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Apply(context.Background(), Config{
		AuthorName:  "Test Bot",
		AuthorEmail: "test@example.com",
	}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	if got, ok := gitConfigGet(t, dir, "user.name"); !ok || got != "Test Bot" {
		t.Errorf("user.name = %q (ok=%v), want %q", got, ok, "Test Bot")
	}
	if got, ok := gitConfigGet(t, dir, "user.email"); !ok || got != "test@example.com" {
		t.Errorf("user.email = %q (ok=%v), want %q", got, ok, "test@example.com")
	}
	if got, ok := gitConfigGet(t, dir, "init.defaultBranch"); !ok || got != "main" {
		t.Errorf("init.defaultBranch = %q (ok=%v), want main", got, ok)
	}
}

func TestApplyDefaultsWhenZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Apply(context.Background(), Config{}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if got, _ := gitConfigGet(t, dir, "user.name"); got != "AI Team" {
		t.Errorf("user.name = %q, want default %q", got, "AI Team")
	}
	if got, _ := gitConfigGet(t, dir, "user.email"); got != "ai@example.com" {
		t.Errorf("user.email = %q, want default %q", got, "ai@example.com")
	}
}

func TestApplyCredentialHelperPresent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Apply(context.Background(), Config{
		Host:     "git.example.com",
		Scheme:   "https",
		HasToken: true,
	}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	got, ok := gitConfigGet(t, dir, "credential.https://git.example.com.helper")
	if !ok {
		t.Fatalf("credential helper key absent, want present")
	}
	if got != credentialHelperValue {
		t.Errorf("credential helper value = %q, want %q", got, credentialHelperValue)
	}
}

func TestApplyCredentialHelperAbsentWithoutHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Apply(context.Background(), Config{HasToken: true}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if _, ok := gitConfigGet(t, dir, "credential.https://.helper"); ok {
		t.Errorf("credential helper present, want absent when Host empty")
	}
}

func TestApplyCredentialHelperAbsentWithoutToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := Apply(context.Background(), Config{Host: "git.example.com"}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if _, ok := gitConfigGet(t, dir, "credential.https://git.example.com.helper"); ok {
		t.Errorf("credential helper present, want absent when no token")
	}
}

// TestHelperReadsEnv proves the configured inline helper emits the username and
// password from the live environment at call time. We invoke the stored helper
// value directly via the shell (the same way git would: `sh -c "<value> get"`),
// which is deterministic and unaffected by any other configured helpers.
func TestHelperReadsEnv(t *testing.T) {
	// git runs the helper as `sh -c '<value-without-leading-!> <operation>'`. The
	// leading "!" is git's own marker (run via shell) and is stripped before the
	// command reaches the shell, so we strip it here too.
	shellCmd := strings.TrimPrefix(credentialHelperValue, "!")
	//nolint:gosec,noctx // G204/noctx: test invokes git/sh with controlled args; no ctx needed in a test helper
	cmd := exec.Command("sh", "-c", shellCmd+" get")
	cmd.Env = []string{"GIT_USER=alice", "GIT_TOKEN=s3cr3t"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper invocation error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "username=alice") {
		t.Errorf("helper output missing username; got %q", s)
	}
	if !strings.Contains(s, "password=s3cr3t") {
		t.Errorf("helper output missing password; got %q", s)
	}
}
