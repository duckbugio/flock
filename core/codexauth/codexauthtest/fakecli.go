// Package codexauthtest provides the shared stand-in for the Codex CLI used by
// every test that drives a device login — core/codexauth's own, and both
// adapters'.
//
// It exists so the captured CLI output has ONE home. The banner below is real,
// copied verbatim from `codex login --device-auth`, ANSI escapes included: those
// escapes are half of what the parser has to survive, so a simplified local copy
// would let an adapter test pass against output the production parser would never
// see. With three copies, a change to Codex's output meant three edits and two
// chances to be quietly left behind.
package codexauthtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The verification link and one-time code the banner carries.
const (
	DeviceURL  = "https://auth.openai.com/codex/device"
	DeviceCode = "XER9-NWCA2"
)

// Banner is what `codex login --device-auth` really prints, captured from the
// CLI: an ANSI-colored preamble, the verification URL, and the one-time code on
// its own line. The escapes are REAL — surviving them is the point.
const Banner = "\n" +
	"Welcome to Codex [v\x1b[90m0.146.0\x1b[0m]\n" +
	"\x1b[90mOpenAI's command-line coding agent\x1b[0m\n" +
	"\n" +
	"Follow these steps to sign in with ChatGPT using device code authorization:\n" +
	"\n" +
	"1. Open this link in your browser and sign in to your account\n" +
	"   \x1b[94m" + DeviceURL + "\x1b[0m\n" +
	"\n" +
	"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n" +
	"   \x1b[94m" + DeviceCode + "\x1b[0m\n"

// PrintBanner is the shell snippet emitting Banner verbatim. A quoted heredoc
// keeps the apostrophes and escapes intact without shell interpretation.
const PrintBanner = "cat <<'CODEX_BANNER_EOF'" + Banner + "CODEX_BANNER_EOF\n"

// Polling is a fake CLI that issues the prompt and then blocks, like the real one
// waiting on the browser step.
const Polling = PrintBanner + "sleep 30\n"

// execPerm makes the written stand-in runnable, which is the whole point of it.
const execPerm os.FileMode = 0o755

// WriteCLI writes an executable stand-in for the Codex CLI whose body is the
// given shell script and returns its path. The script receives the same
// arguments the real CLI would.
func WriteCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), execPerm); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

// promptWait bounds how long AwaitCode waits for the fake CLI's prompt.
const promptWait = 10 * time.Second

// AwaitCode drains delivered notices until the one carrying DeviceCode and
// returns it. A pending login delivers its acknowledgement through the SAME
// channel as the prompt, and deliberately ahead of it, so a caller that wants the
// code has to skip past what precedes it.
//
// This package stops at the fixtures on purpose: a helper that built a
// codexauth.Manager would have to import codexauth, and codexauth's own tests
// import this package — that is an import cycle. What actually drifts (the
// captured banner and its tokens) is shared; the few lines of manager glue stay
// with each caller.
func AwaitCode(t *testing.T, notices <-chan string) string {
	t.Helper()
	deadline := time.After(promptWait)
	for {
		select {
		case text := <-notices:
			if strings.Contains(text, DeviceCode) {
				return text
			}
		case <-deadline:
			t.Fatal("the fake login never issued a prompt")
			return ""
		}
	}
}
