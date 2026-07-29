// Package codexauth drives the Codex CLI's DEVICE-CODE login from a chat, so a
// headless deploy (container, VPS) can be authorized without a shell on the host.
//
// Why device code and not plain `codex login`: the plain flow starts a loopback
// OAuth server on http://localhost:1455 and prints the authorize URL whose
// redirect_uri points back at that loopback. Inside a container nobody can reach
// it — the operator opens the URL on their laptop, the callback lands on THEIR
// localhost, and the CLI waits for a callback that never arrives. That is the
// "no code is shown and the CLI hangs forever" symptom. Codex itself says so on
// stderr ("On a remote or headless machine? Use `codex login --device-auth`").
//
// `codex login --device-auth` instead prints, on STDOUT, a verification URL plus
// a one-time code and then BLOCKS, polling the token endpoint until the user
// finishes in a browser. Two consequences shape this package:
//
//  1. The URL and code must be read INCREMENTALLY from the live pipe. Anything
//     that waits for the process to exit (cmd.Output/CombinedOutput) shows
//     nothing at all until the login is already over — the same apparent hang.
//  2. Codex colors that output with ANSI escapes even when stdout is not a TTY,
//     so the text has to be de-escaped before the URL and code can be matched.
package codexauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DeviceCodeTTL is how long Codex says a one-time code stays valid. The login
// process is capped slightly above it so an abandoned login cannot leak a child
// process for longer than the code it is waiting on.
const DeviceCodeTTL = 15 * time.Minute

// killGrace is how long the child gets to exit after SIGTERM before SIGKILL.
const killGrace = 500 * time.Millisecond

// outputTailBytes is how much CLI output we keep to enrich a failure message.
const outputTailBytes = 4096

// initScanBuf is the line scanner's initial buffer size, and maxScanLine caps a
// single output line so a pathological stream cannot grow it without bound.
const (
	initScanBuf = 4096
	maxScanLine = 1 << 20
)

// ErrDeviceAuthUnsupported reports a Codex CLI too old to know --device-auth.
// Without it there is no headless login path at all, so the operator has to
// upgrade the CLI rather than retry.
var ErrDeviceAuthUnsupported = errors.New(
	"this Codex CLI does not support `codex login --device-auth`; upgrade the Codex CLI to authorize headlessly")

// ErrNoPrompt reports that Codex exited before printing a verification URL and
// one-time code. The wrapped message carries the captured CLI output.
var ErrNoPrompt = errors.New("codex printed no device-login code")

// Prompt is the user-actionable half of a device login: open URL in a browser,
// sign in, and enter Code there. ExpiresIn is Codex's own stated validity window
// (zero when it did not state one).
type Prompt struct {
	URL       string
	Code      string
	ExpiresIn time.Duration
}

// Login runs `codex login --device-auth` to completion and reports the outcome.
//
// onPrompt is called ONCE, as soon as the verification URL and one-time code have
// been parsed off the live output — long before the call returns — so the caller
// can relay them to the user while the CLI keeps polling. It may be nil. It runs
// ON the output-reading goroutine, which keeps it strictly ordered before the
// outcome the caller reports afterwards; in exchange it must not block
// indefinitely (adapters send their notice under a bounded context).
//
// Login blocks until the user finishes in the browser (nil), until Codex fails
// (error), or until ctx is done. On ctx cancellation the whole child process
// GROUP is signalled, so no polling child is left behind, and ctx.Err() is
// returned. Success is defined by Codex exiting 0, which is also when auth.json
// under CODEX_HOME has been written.
func Login(ctx context.Context, cfg Config, onPrompt func(Prompt)) error {
	//nolint:gosec,noctx // bin is operator config; ctx drives the process-group kill below.
	cmd := exec.Command(cfg.bin(), "login", "--device-auth")
	cmd.Env = cfg.childEnv()

	// The device flow needs no stdin, but an inherited terminal would let the CLI
	// block on a read. A closed stdin keeps it strictly non-interactive.
	cmd.Stdin = nil
	// Own process group: cancelling must kill the poller, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codexauth: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("codexauth: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codexauth: start %s: %w", cfg.bin(), err)
	}

	done := make(chan struct{})
	var (
		killMu    sync.Mutex
		killTimer *time.Timer
	)
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		select {
		case <-ctx.Done():
			t := killGroup(cmd)
			killMu.Lock()
			killTimer = t
			killMu.Unlock()
		case <-done:
		}
	}()

	// Both streams are parsed: --device-auth prints the prompt on stdout, while
	// diagnostics (including the "use --device-auth" hint and argument errors)
	// arrive on stderr. Whichever carries the URL and code, we find it.
	out := newTail(outputTailBytes)
	var parseMu sync.Mutex
	parser := &promptParser{}
	var streams sync.WaitGroup
	for _, rd := range []io.Reader{stdout, stderr} {
		streams.Add(1)
		go func(rd io.Reader) {
			defer streams.Done()
			scanLines(rd, func(line string) {
				out.WriteString(line + "\n")
				parseMu.Lock()
				p, ok := parser.feed(line)
				parseMu.Unlock()
				if ok && onPrompt != nil {
					onPrompt(p)
				}
			})
		}(rd)
	}
	streams.Wait()

	waitErr := cmd.Wait()
	close(done)
	watch.Wait()
	killMu.Lock()
	if killTimer != nil {
		killTimer.Stop()
	}
	killMu.Unlock()

	parseMu.Lock()
	sawPrompt := parser.sent
	parseMu.Unlock()

	return loginResult(ctx, waitErr, out.String(), sawPrompt)
}

// loginResult maps the child's exit onto the caller-facing error contract: a
// cancelled/expired ctx wins over any exit status (the kill caused it), an
// unknown --device-auth flag is reported as its own sentinel so the caller can
// tell the operator to upgrade, and a clean exit that never printed a prompt is
// still a failure — the user was never given anything to act on.
func loginResult(ctx context.Context, waitErr error, output string, sawPrompt bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isUnsupportedFlag(output) {
		return ErrDeviceAuthUnsupported
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			return fmt.Errorf("codex login exited with code %d: %s", ee.ExitCode(), condense(output))
		}
		return fmt.Errorf("codex login failed: %w: %s", waitErr, condense(output))
	}
	if !sawPrompt {
		return fmt.Errorf("%w: %s", ErrNoPrompt, condense(output))
	}
	return nil
}

// isUnsupportedFlag detects a Codex CLI that does not know --device-auth. clap
// (the CLI parser Codex uses) words this as "unexpected argument"; older/other
// builds say "unrecognized". Matching either keeps the check robust without
// pinning a version.
func isUnsupportedFlag(output string) bool {
	low := strings.ToLower(output)
	if !strings.Contains(low, "device-auth") {
		return false
	}
	return strings.Contains(low, "unexpected argument") ||
		strings.Contains(low, "unrecognized") ||
		strings.Contains(low, "unknown flag") ||
		strings.Contains(low, "invalid value")
}

// condense collapses captured CLI output into one line for an error message,
// dropping blank lines so a mostly-decorative banner does not bury the cause.
func condense(output string) string {
	fields := strings.FieldsFunc(output, func(r rune) bool { return r == '\n' || r == '\r' })
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return "(no output)"
	}
	return strings.Join(kept, " | ")
}

// scanLines feeds rd's output to onLine one ANSI-stripped line at a time,
// splitting on \n AND \r so a progress line rewritten in place still surfaces.
// It returns when rd hits EOF.
func scanLines(rd io.Reader, onLine func(string)) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, initScanBuf), maxScanLine)
	sc.Split(splitLines)
	for sc.Scan() {
		line := stripANSI(sc.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		onLine(line)
	}
}

// splitLines is a bufio.SplitFunc that breaks on either \n or \r, so output the
// CLI redraws with a carriage return is not withheld until the next newline.
func splitLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ansiRe matches CSI/OSC-style escape sequences. Codex colors its login output
// even when stdout is a pipe, so the raw text carries escapes around exactly the
// two tokens we need (the URL and the code) and must be cleaned before matching.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// stripANSI removes terminal escape sequences from s.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

var (
	// urlRe matches the verification link. Trailing punctuation is trimmed by the
	// character class so a URL at the end of a sentence stays intact.
	urlRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
	// codeRe matches a one-time code such as "XER9-NWCA2".
	codeRe = regexp.MustCompile(`\b[A-Z0-9]{4,8}-[A-Z0-9]{4,8}\b`)
	// ttlRe matches Codex's stated validity window, e.g. "(expires in 15 minutes)".
	ttlRe = regexp.MustCompile(`(?i)expires in (\d+) (second|minute|hour)s?`)
)

// promptParser accumulates the device-login prompt across the CLI's output
// lines. Codex prints the URL and the code on separate lines, so neither alone
// is actionable; the prompt is released exactly once, on the line that completes
// the pair.
type promptParser struct {
	url  string
	code string
	ttl  time.Duration
	sent bool
}

// feed consumes one ANSI-stripped output line. ok is true only for the line that
// first completes the URL+code pair.
func (p *promptParser) feed(line string) (Prompt, bool) {
	if p.sent {
		return Prompt{}, false
	}
	if p.url == "" {
		if m := urlRe.FindString(line); m != "" {
			p.url = strings.TrimRight(m, ".,;")
		}
	}
	if p.code == "" {
		if c, ok := matchCode(line); ok {
			p.code = c
		}
	}
	if p.ttl == 0 {
		p.ttl = matchTTL(line)
	}
	if p.url == "" || p.code == "" {
		return Prompt{}, false
	}
	p.sent = true
	return Prompt{URL: p.url, Code: p.code, ExpiresIn: p.ttl}, true
}

// matchCode extracts a one-time code from line. A bare uppercase hyphenated
// token is only accepted when it is the WHOLE line (how Codex prints it) or when
// the line is explicitly about a code — otherwise an unrelated token in a banner
// or inside a URL could be mistaken for one.
func matchCode(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if urlRe.MatchString(trimmed) {
		return "", false
	}
	m := codeRe.FindString(trimmed)
	if m == "" {
		return "", false
	}
	if m == trimmed || strings.Contains(strings.ToLower(trimmed), "code") {
		return m, true
	}
	return "", false
}

// matchTTL reads Codex's stated code lifetime off a line, returning 0 when the
// line does not state one.
func matchTTL(line string) time.Duration {
	m := ttlRe.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "second":
		return time.Duration(n) * time.Second
	case "hour":
		return time.Duration(n) * time.Hour
	default:
		return time.Duration(n) * time.Minute
	}
}

// killGroup signals the child's whole process group, escalating to SIGKILL after
// killGrace. It mirrors core/codex's runner so a cancelled login cannot leave a
// polling Codex process behind.
func killGroup(cmd *exec.Cmd) *time.Timer {
	if cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	return time.AfterFunc(killGrace, func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}

// tail is a bounded io.Writer keeping only the last max bytes written, used to
// enrich failures with the CLI's own words without unbounded buffering.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTail(limit int) *tail { return &tail{max: limit} }

// WriteString appends s, dropping the oldest bytes past the cap.
func (t *tail) WriteString(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, s...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
}

// String returns the retained tail.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
