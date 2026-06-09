// Package claude runs a prompt through the `claude` CLI and exposes its
// stream-json output as a typed Go event stream. The CLI is the engine; this
// package only spawns it, streams its events, and manages its lifecycle. Each
// Run is a one-shot invocation per message (no long-lived process); continuity
// across messages is provided by --resume <session_id>, not by a persistent
// child.
package claude

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// EventType discriminates the kinds of Event emitted on a run's channel.
type EventType int

const (
	// SystemInit is emitted once for the system/init envelope; SessionID is set.
	SystemInit EventType = iota
	// Text is emitted for each assistant text content block; Text is set.
	Text
	// ToolUse is emitted for each assistant tool_use block; Tool and ToolInput are set.
	ToolUse
	// ToolResult is emitted when a tool finishes (user/tool_result envelope).
	ToolResult
	// Result is the terminal event for a successful stream; Result is set.
	Result
	// RunError is emitted when the process fails without a result envelope; Err is set.
	RunError
)

// Event is a single typed item from a run's event stream. Only the fields
// relevant to Type are populated.
type Event struct {
	Type      EventType
	Text      string          // for Text
	Tool      string          // for ToolUse (e.g. "Bash")
	ToolInput json.RawMessage // for ToolUse
	SessionID string          // set on SystemInit
	Result    *RunResult      // set on Result
	Err       error           // set on RunError
}

// RunResult is the parsed terminal result envelope of a run.
type RunResult struct {
	Text       string
	SessionID  string
	NumTurns   int
	CostUSD    float64
	DurationMS int64
	IsError    bool
	Subtype    string
}

// ImageInput is a single image attachment for a run. When a run carries one or
// more images, the prompt is delivered to the CLI as a stream-json user message
// (text + image content blocks on stdin) instead of a trailing prompt arg.
type ImageInput struct {
	// MediaType is the image MIME type (e.g. "image/jpeg", "image/png").
	MediaType string
	// Data is the raw (un-encoded) image bytes; base64-encoded into the envelope.
	Data []byte
}

// Options configures a single Run.
type Options struct {
	SessionID string   // resume an existing session; omit --resume when empty
	Workdir   string   // working/allowed dir; also the child's cwd
	Model     string   // --model value
	MaxTurns  int      // --max-turns value
	Env       []string // child environment (auth, etc.); inherited when nil
	// Images, when non-empty, switches the run to stream-json stdin input: the
	// prompt and these image blocks are written as one user message to the child's
	// stdin (then stdin is closed), enabling Claude vision. When empty the run uses
	// the default trailing-arg text path (fully backward compatible).
	Images []ImageInput
}

// Runner runs prompts through the claude CLI.
type Runner interface {
	// Run spawns the CLI for prompt and returns a channel of typed events. The
	// channel is closed when the run terminates (result, error, or context
	// cancellation). The error return is non-nil only when the process could not
	// be started at all.
	Run(ctx context.Context, prompt string, o Options) (<-chan Event, error)
}

// New returns a Runner that invokes the claude binary at claudeBin.
func New(claudeBin string) Runner {
	return &runner{bin: claudeBin}
}

type runner struct {
	bin string
}

// killGrace is how long the child gets to exit after SIGTERM before SIGKILL.
const killGrace = 500 * time.Millisecond

// maxScanLine is the per-line buffer cap for the stdout scanner. Stream-json
// events (long assistant text, big tool inputs) routinely exceed bufio's 64KB
// default, so we raise it well above that.
const maxScanLine = 16 * 1024 * 1024

// stderrTailBytes is how much trailing stderr we keep to enrich errors.
const stderrTailBytes = 4096

func (r *runner) Run(ctx context.Context, prompt string, o Options) (<-chan Event, error) {
	// A run with image attachments is delivered via stdin as a stream-json user
	// message; a plain text run keeps the trailing-prompt-arg path unchanged.
	stdinMode := len(o.Images) > 0

	args := buildArgs(o, prompt, stdinMode)

	// exec.Command (not CommandContext) is deliberate: cancellation is handled in
	// stream() by killing the whole process GROUP, since CommandContext would only
	// signal the direct child and leak the CLI's Node + tool subprocesses.
	//nolint:gosec,noctx // bin+args are caller-controlled; ctx drives a process-group kill in stream().
	cmd := exec.Command(r.bin, args...)
	cmd.Dir = o.Workdir
	if len(o.Env) > 0 {
		cmd.Env = o.Env
	}
	// Run the child in its own process group so context cancellation can kill
	// the whole tree (the CLI spawns Node + tool subprocesses).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var (
		stdin    io.WriteCloser
		envelope []byte
	)
	if stdinMode {
		var err error
		envelope, err = userMessageEnvelope(prompt, o.Images)
		if err != nil {
			return nil, fmt.Errorf("build user message: %w", err)
		}
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("stdin pipe: %w", err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrTail := newTail(stderrTailBytes)
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", r.bin, err)
	}

	if stdinMode {
		// Write the user-message envelope and close stdin so the CLI processes the
		// single message and exits (one-shot, like the trailing-arg path). The write
		// runs on its own goroutine so a slow/blocked child can't stall Run's return.
		// Write errors are non-fatal here: the stream/parse path surfaces a RunError
		// if the child produced no result.
		go func() {
			_, _ = stdin.Write(envelope)
			_ = stdin.Close()
		}()
	}

	out := make(chan Event)
	go r.stream(ctx, cmd, stdout, stderrTail, out)
	return out, nil
}

// buildArgs assembles the CLI argument list per §4 of the rewrite plan. When
// stdinMode is true the prompt is delivered on stdin as a stream-json user
// message (for image attachments), so the trailing prompt arg is omitted and the
// input format is set to stream-json; otherwise the prompt is the trailing arg.
func buildArgs(o Options, prompt string, stdinMode bool) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
	}
	if stdinMode {
		args = append(args, "--input-format", "stream-json")
	}
	// --add-dir is VARIADIC in the claude CLI (`--add-dir <directories...>`): it
	// greedily consumes EVERY following non-flag token. It must therefore be
	// emitted BEFORE the single-value flags below and never be the last flag — else
	// the trailing [prompt] positional (text path) is swallowed as a bogus extra
	// directory, leaving the CLI with no input ("Input must be provided either
	// through stdin or as a prompt argument when using --print"). Keeping a flag
	// (--permission-mode at minimum) after its value stops the variadic in time.
	if o.Workdir != "" {
		args = append(args, "--add-dir", o.Workdir)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(o.MaxTurns))
	}
	args = append(args, "--permission-mode", "bypassPermissions")
	if o.SessionID != "" {
		args = append(args, "--resume", o.SessionID)
	}
	if !stdinMode {
		args = append(args, prompt)
	}
	return args
}

// userMessageEnvelope builds the single newline-terminated stream-json "user"
// message carrying the prompt text plus base64 image content blocks, matching the
// CLI's --input-format stream-json schema.
func userMessageEnvelope(prompt string, images []ImageInput) ([]byte, error) {
	content := make([]inputBlock, 0, 1+len(images))
	content = append(content, inputBlock{Type: "text", Text: prompt})
	for _, img := range images {
		content = append(content, inputBlock{
			Type: "image",
			Source: &inputImageSource{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	env := inputEnvelope{Type: roleUser}
	env.Message.Role = roleUser
	env.Message.Content = content
	b, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// inputEnvelope / inputBlock / inputImageSource mirror the stream-json
// user-message schema the CLI accepts on stdin with --input-format stream-json.
// They are distinct from the OUTPUT decode types (decode.go) which model the
// CLI's stdout.
type inputEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Role    string       `json:"role"`
		Content []inputBlock `json:"content"`
	} `json:"message"`
}

type inputBlock struct {
	Type   string            `json:"type"`
	Text   string            `json:"text,omitempty"`
	Source *inputImageSource `json:"source,omitempty"`
}

type inputImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"` //nolint:tagliatelle // Claude CLI emits snake_case.
	Data      string `json:"data"`
}

// stream parses the child's stdout, emits events, manages cancellation, and
// closes out exactly once before returning.
func (r *runner) stream(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, stderrTail *tail, out chan<- Event) {
	defer close(out)

	// Watch ctx: on cancellation, kill the whole process group (SIGTERM grace,
	// then SIGKILL) so the child dies within ~1s. Stop the watcher once parsing
	// is done to avoid leaking the goroutine.
	done := make(chan struct{})
	// killTimer holds the pending SIGKILL escalation scheduled by killGroup, so
	// we can cancel it once the child is reaped (see below). Guarded by killMu
	// because it is set on the watcher goroutine and read on this one.
	var (
		killMu    sync.Mutex
		killTimer *time.Timer
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			t := killGroup(cmd)
			killMu.Lock()
			killTimer = t
			killMu.Unlock()
		case <-done:
		}
	}()
	defer func() {
		close(done)
		wg.Wait()
	}()

	sawResult, scanErr := r.parse(ctx, stdout, out)

	waitErr := cmd.Wait()
	// The child has been reaped, so its PGID is no longer ours. Cancel any
	// pending SIGKILL escalation to avoid signalling a process group the OS may
	// have recycled to an unrelated process.
	killMu.Lock()
	if killTimer != nil {
		killTimer.Stop()
	}
	killMu.Unlock()

	// Context cancellation: report nothing further; the caller knows it cancelled.
	if ctx.Err() != nil {
		return
	}
	if sawResult {
		return
	}
	// No terminal result and the process ended (cleanly or not): surface an error
	// enriched with the exit code and stderr tail. If the scanner itself failed
	// (e.g. an over-cap line), fold that in so we don't misreport a truncated
	// stream as a clean "ended without a result event".
	// Note: the error may carry the tail of the child's stderr, which is raw CLI
	// output; callers that surface it to end users should treat it accordingly.
	emit(ctx, out, Event{Type: RunError, Err: exitError(waitErr, stderrTail, scanErr)})
}

// parse scans stdout line by line, decodes each envelope, and emits events. It
// returns whether a terminal result envelope was seen and the scanner's error
// (nil on a clean EOF), so the caller can fold a scan failure into the surfaced
// RunError when no result was reached.
func (r *runner) parse(ctx context.Context, stdout io.Reader, out chan<- Event) (bool, error) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)

	var sawResult bool
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, ok := decode(line)
		if !ok {
			continue
		}
		for _, e := range ev {
			if e.Type == Result {
				sawResult = true
			}
			if !emit(ctx, out, e) {
				return sawResult, sc.Err()
			}
		}
	}
	return sawResult, sc.Err()
}

// emit sends e on out unless ctx is cancelled; it reports whether the send
// happened (false means the run is being torn down).
func emit(ctx context.Context, out chan<- Event, e Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// killGroup sends SIGTERM to the child's process group, then schedules a SIGKILL
// after a short grace so the whole tree dies promptly. It returns the escalation
// timer (nil if there was no process to signal) so the caller can Stop() it once
// the child is reaped — otherwise a late SIGKILL could hit a recycled PGID.
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

// exitError builds an error describing a non-zero / unexpected exit, enriched
// with the exit code and the tail of stderr. scanErr, when non-nil, is the
// stdout scanner's failure (e.g. an over-cap line) and is folded in so a
// truncated stream isn't misreported as a clean "ended without a result event".
func exitError(waitErr error, stderrTail *tail, scanErr error) error {
	tail := stderrTail.String()
	var ee *exec.ExitError
	switch {
	case waitErr != nil && asExitError(waitErr, &ee):
		if tail != "" {
			return fmt.Errorf("claude exited with code %d: %s", ee.ExitCode(), tail)
		}
		return fmt.Errorf("claude exited with code %d", ee.ExitCode())
	case waitErr != nil:
		if tail != "" {
			return fmt.Errorf("claude run failed: %w: %s", waitErr, tail)
		}
		return fmt.Errorf("claude run failed: %w", waitErr)
	case scanErr != nil:
		// The process exited cleanly but the stdout scan failed mid-stream, so we
		// likely never saw the result envelope because the stream was truncated.
		if tail != "" {
			return fmt.Errorf("claude stream scan failed before a result event: %w: %s", scanErr, tail)
		}
		return fmt.Errorf("claude stream scan failed before a result event: %w", scanErr)
	default:
		// Process exited 0 but never emitted a result envelope.
		if tail != "" {
			return fmt.Errorf("claude ended without a result event: %s", tail)
		}
		return errors.New("claude ended without a result event")
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
