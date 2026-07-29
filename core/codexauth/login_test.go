//nolint:testpackage // intentionally whitebox to test the device-login parser and manager internals
package codexauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/codexauth/codexauthtest"
)

// The fake CLI, its captured banner and the tokens it carries live in
// codexauthtest so core/codexauth, the VK adapter and the Telegram adapter all
// assert against the SAME real output.
const (
	deviceURL   = codexauthtest.DeviceURL
	deviceCode  = codexauthtest.DeviceCode
	printBanner = codexauthtest.PrintBanner
)

// fakeCodex writes an executable stand-in for the Codex CLI running script.
func fakeCodex(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a POSIX shell script")
	}
	return codexauthtest.WriteCLI(t, script)
}

// testConfig points a Config at the fake CLI with a scratch CODEX_HOME and a
// minimal environment (an empty Env would inherit the test runner's).
func testConfig(t *testing.T, bin string) Config {
	t.Helper()
	return Config{
		Backend:  BackendCodex,
		AuthMode: AuthSubscription,
		Bin:      bin,
		Home:     t.TempDir(),
		Env:      []string{"PATH=" + os.Getenv("PATH")},
	}
}

// TestLoginEmitsPromptWhileProcessStillRunning is the core regression for the
// reported symptom: with `codex login --device-auth` the URL and code are printed
// and the CLI THEN blocks for minutes, polling. Anything that waits for the
// process to exit before reading (cmd.Output/CombinedOutput) shows the user
// nothing and looks like a hang. The prompt must arrive while the child is still
// running.
func TestLoginEmitsPromptWhileProcessStillRunning(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prompts := make(chan Prompt, 1)
	errs := make(chan error, 1)
	go func() {
		errs <- Login(ctx, testConfig(t, bin), func(p Prompt) { prompts <- p })
	}()

	var got Prompt
	select {
	case got = <-prompts:
	case err := <-errs:
		t.Fatalf("Login returned %v before emitting a prompt", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no prompt within 10s: the CLI output is not being read incrementally")
	}

	if got.URL != deviceURL {
		t.Errorf("URL = %q, want the device verification link", got.URL)
	}
	if got.Code != deviceCode {
		t.Errorf("Code = %q, want XER9-NWCA2", got.Code)
	}
	if got.ExpiresIn != 15*time.Minute {
		t.Errorf("ExpiresIn = %v, want 15m", got.ExpiresIn)
	}

	// Cancelling must reap the still-polling child and report the ctx error.
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Login after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Login did not return after cancel: the child was not killed")
	}
}

// TestLoginSuccess: a CLI that prints the prompt and exits 0 is a completed
// sign-in.
func TestLoginSuccess(t *testing.T) {
	bin := fakeCodex(t, printBanner+"exit 0\n")
	var got Prompt
	err := Login(context.Background(), testConfig(t, bin), func(p Prompt) { got = p })
	if err != nil {
		t.Fatalf("Login = %v, want nil", err)
	}
	if got.Code == "" {
		t.Error("onPrompt was never called")
	}
}

// TestLoginDeviceAuthUnsupported: a Codex CLI too old to know --device-auth has
// no headless login path at all, so the caller must be told to upgrade rather
// than to retry.
func TestLoginDeviceAuthUnsupported(t *testing.T) {
	bin := fakeCodex(t, "echo \"error: unexpected argument '--device-auth' found\" >&2\nexit 2\n")
	err := Login(context.Background(), testConfig(t, bin), nil)
	if !errors.Is(err, ErrDeviceAuthUnsupported) {
		t.Fatalf("Login = %v, want ErrDeviceAuthUnsupported", err)
	}
}

// TestLoginNoPrompt: a clean exit that never printed a code is still a failure —
// the user was given nothing to act on — and the error carries the CLI's output.
func TestLoginNoPrompt(t *testing.T) {
	bin := fakeCodex(t, "echo 'nothing useful here'\nexit 0\n")
	err := Login(context.Background(), testConfig(t, bin), nil)
	if !errors.Is(err, ErrNoPrompt) {
		t.Fatalf("Login = %v, want ErrNoPrompt", err)
	}
	if !strings.Contains(err.Error(), "nothing useful here") {
		t.Errorf("error %q should quote the CLI output", err)
	}
}

// TestLoginFailureQuotesOutput: a non-zero exit reports the code and the CLI's
// own words, including stderr.
func TestLoginFailureQuotesOutput(t *testing.T) {
	bin := fakeCodex(t, "echo 'network unreachable' >&2\nexit 7\n")
	err := Login(context.Background(), testConfig(t, bin), nil)
	if err == nil {
		t.Fatal("Login = nil, want an error")
	}
	if !strings.Contains(err.Error(), "7") || !strings.Contains(err.Error(), "network unreachable") {
		t.Errorf("error %q should carry the exit code and stderr", err)
	}
}

// TestLoginForcesConfiguredCodexHome: the login must write auth.json where the
// RUNNER later looks for it. An inherited CODEX_HOME pointing elsewhere would
// leave the deployment "logged in" and unauthorized at the same time.
func TestLoginForcesConfiguredCodexHome(t *testing.T) {
	bin := fakeCodex(t, printBanner+"echo \"$CODEX_HOME\" > \"$CODEX_HOME/seen\"\nexit 0\n")
	cfg := testConfig(t, bin)
	cfg.Env = append(cfg.Env, "CODEX_HOME=/somewhere/else")

	if err := Login(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Login = %v, want nil", err)
	}
	seen, err := os.ReadFile(filepath.Join(cfg.Home, "seen"))
	if err != nil {
		t.Fatalf("read CODEX_HOME probe: %v", err)
	}
	if strings.TrimSpace(string(seen)) != cfg.Home {
		t.Errorf("child CODEX_HOME = %q, want %q", strings.TrimSpace(string(seen)), cfg.Home)
	}
}

// TestLoginTimeoutReported: the device code expires, so an abandoned login ends
// with the deadline rather than hanging forever.
func TestLoginTimeoutReported(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := Login(ctx, testConfig(t, bin), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Login = %v, want context.DeadlineExceeded", err)
	}
}

// TestLoginMissingBinary: an unusable CODEX_BIN fails fast at start rather than
// leaving the caller waiting on a process that never existed.
func TestLoginMissingBinary(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "does-not-exist"))
	if err := Login(context.Background(), cfg, nil); err == nil {
		t.Fatal("Login = nil, want a start error")
	}
}

// TestStripANSI: Codex colors its login output even when stdout is a pipe, so the
// URL and code arrive wrapped in escapes.
func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"colored code", "   \x1b[94mXER9-NWCA2\x1b[0m", "   XER9-NWCA2"},
		{"plain text", "no escapes here", "no escapes here"},
		{"osc sequence", "\x1b]0;title\x07done", "done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripANSI(tt.in); got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPromptParser covers the pairing rule: neither the URL nor the code alone is
// actionable, so the prompt is released exactly once, on the line completing the
// pair, and never again.
func TestPromptParser(t *testing.T) {
	p := &promptParser{}
	if _, ok := p.feed("1. Open this link"); ok {
		t.Fatal("a prose line released a prompt")
	}
	if _, ok := p.feed("   https://auth.openai.com/codex/device"); ok {
		t.Fatal("the URL alone released a prompt")
	}
	if _, ok := p.feed("2. Enter this one-time code (expires in 15 minutes)"); ok {
		t.Fatal("the TTL line alone released a prompt")
	}
	got, ok := p.feed("   XER9-NWCA2")
	if !ok {
		t.Fatal("the code line did not release the prompt")
	}
	if got.URL != deviceURL || got.Code != deviceCode {
		t.Errorf("prompt = %+v, want the parsed URL and code", got)
	}
	if got.ExpiresIn != 15*time.Minute {
		t.Errorf("ExpiresIn = %v, want 15m", got.ExpiresIn)
	}
	if _, ok := p.feed("   ABCD-EFGH"); ok {
		t.Error("a second prompt was released")
	}
}

// TestMatchCode pins the anti-false-positive rule: a hyphenated uppercase token is
// a code only when it is the whole line or the line is explicitly about a code —
// never when it comes out of a URL or an unrelated banner.
func TestMatchCode(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"bare code line", "   XER9-NWCA2  ", deviceCode},
		{"labelled code", "Your one-time code: XER9-NWCA2 now", deviceCode},
		{"inside a url", "https://example.com/AAAA-BBBB", ""},
		{"unrelated banner token", "Release NOTES-DRAFT ready", ""},
		{"lowercase", "   xer9-nwca2", ""},
		{"no hyphen", "   XER9NWCA2", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchCode(tt.line)
			if tt.want == "" {
				if ok {
					t.Errorf("matchCode(%q) = %q, want no match", tt.line, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Errorf("matchCode(%q) = %q/%v, want %q", tt.line, got, ok, tt.want)
			}
		})
	}
}

// TestMatchTTL reads Codex's stated code lifetime in each unit it might use.
func TestMatchTTL(t *testing.T) {
	tests := []struct {
		line string
		want time.Duration
	}{
		{"2. Enter this one-time code (expires in 15 minutes)", 15 * time.Minute},
		{"expires in 1 minute", time.Minute},
		{"expires in 90 seconds", 90 * time.Second},
		{"expires in 2 hours", 2 * time.Hour},
		{"no lifetime here", 0},
		{"expires in 0 minutes", 0},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := matchTTL(tt.line); got != tt.want {
				t.Errorf("matchTTL(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestIsUnsupportedFlag distinguishes "this CLI cannot do device auth" from any
// other failure that merely mentions the flag.
func TestIsUnsupportedFlag(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"clap wording", "error: unexpected argument '--device-auth' found", true},
		{"getopt wording", "unrecognized option: --device-auth", true},
		{"unrelated failure", "device-auth polling failed: network unreachable", false},
		{"other flag", "error: unexpected argument '--nope' found", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnsupportedFlag(tt.output); got != tt.want {
				t.Errorf("isUnsupportedFlag(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

// TestSplitLinesBreaksOnCarriageReturn: a line the CLI redraws in place with \r
// must surface immediately instead of waiting for a newline that may never come.
func TestSplitLinesBreaksOnCarriageReturn(t *testing.T) {
	advance, token, err := splitLines([]byte("waiting…\rdone\n"), false)
	if err != nil {
		t.Fatalf("splitLines err = %v", err)
	}
	if string(token) != "waiting…" {
		t.Errorf("token = %q, want %q", token, "waiting…")
	}
	if advance != len("waiting…")+1 {
		t.Errorf("advance = %d, want %d", advance, len("waiting…")+1)
	}
}

// TestLoginIgnoresAStrayURLFromTheOtherStream is the anti-mispairing guard. The
// two output streams are read by separate goroutines, so a shared parser would
// interleave them nondeterministically: an unrelated URL on stderr (an upgrade
// notice, a policy link) could be paired with the REAL code from stdout, and the
// user would be sent to the wrong page holding a code that works. The URL and
// the code must come from the same stream.
func TestLoginIgnoresAStrayURLFromTheOtherStream(t *testing.T) {
	bin := fakeCodex(t, "echo 'A new version is available: https://example.com/upgrade' >&2\n"+
		"sleep 0.2\n"+printBanner+"exit 0\n")

	var got Prompt
	if err := Login(context.Background(), testConfig(t, bin), func(p Prompt) { got = p }); err != nil {
		t.Fatalf("Login = %v, want nil", err)
	}
	if got.URL != deviceURL {
		t.Errorf("URL = %q, want the verification link, not the stray stderr one", got.URL)
	}
	if got.Code != deviceCode {
		t.Errorf("Code = %q, want XER9-NWCA2", got.Code)
	}
}

// TestPromptParserPrefersTheVerificationURL: within one stream, a URL printed
// before the verification link must not win merely by being first.
func TestPromptParserPrefersTheVerificationURL(t *testing.T) {
	p := &promptParser{}
	p.feed("Docs: https://example.com/help")
	p.feed("   https://auth.openai.com/codex/device")
	got, ok := p.feed("   XER9-NWCA2")
	if !ok {
		t.Fatal("no prompt released")
	}
	if got.URL != deviceURL {
		t.Errorf("URL = %q, want the device link to outrank the earlier one", got.URL)
	}
}

// TestPromptParserKeepsALoneUnrecognizedURL: the preference is a ranking, not a
// filter — if Codex ever moves off these domains, the only URL on offer is still
// used rather than the flow breaking outright.
func TestPromptParserKeepsALoneUnrecognizedURL(t *testing.T) {
	p := &promptParser{}
	p.feed("   https://login.example.test/activate")
	got, ok := p.feed("   XER9-NWCA2")
	if !ok {
		t.Fatal("no prompt released")
	}
	if got.URL != "https://login.example.test/activate" {
		t.Errorf("URL = %q, want the single available URL", got.URL)
	}
}

// TestURLScore pins the ranking.
func TestURLScore(t *testing.T) {
	tests := []struct {
		url  string
		want int
	}{
		{deviceURL, scoreDevicePath},
		{"https://chatgpt.com/codex/settings", scoreKnownHost},
		{"https://example.com/upgrade", scoreUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := urlScore(tt.url); got != tt.want {
				t.Errorf("urlScore(%q) = %d, want %d", tt.url, got, tt.want)
			}
		})
	}
}

// TestErrorsRedactTheOneTimeCode: a failure carries the CLI's own output, which
// is logged and may be shown in a chat — and that output contains a live
// credential. Anyone who reads the code out of a log can complete the sign-in
// with their own account, so it must never get that far.
func TestErrorsRedactTheOneTimeCode(t *testing.T) {
	bin := fakeCodex(t, printBanner+"echo 'polling failed' >&2\nexit 4\n")

	err := Login(context.Background(), testConfig(t, bin), nil)
	if err == nil {
		t.Fatal("Login = nil, want an error")
	}
	if strings.Contains(err.Error(), deviceCode) {
		t.Errorf("the one-time code leaked into an error: %q", err)
	}
	if strings.Contains(err.Error(), deviceURL) {
		t.Errorf("the verification link leaked into an error: %q", err)
	}
	if !strings.Contains(err.Error(), "polling failed") {
		t.Errorf("error %q lost the CLI's actual diagnosis", err)
	}
}

// TestRedactKeepsTheDiagnosis: redaction must not swallow the message that makes
// the failure debuggable.
func TestRedactKeepsTheDiagnosis(t *testing.T) {
	got := redact("visit " + deviceURL + " and enter " + deviceCode + " — connection refused")
	if strings.Contains(got, deviceCode) || strings.Contains(got, deviceURL) {
		t.Errorf("redact left a secret in %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("redact(%q) dropped the diagnosis", got)
	}
}
