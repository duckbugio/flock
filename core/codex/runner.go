// Package codex runs prompts through the Codex CLI and adapts its JSONL stream
// to the common agent.Runner event contract used by chat.Service.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/duckbugio/flock/core/agent"
)

const (
	// AuthSubscription uses Codex login state from CODEX_HOME or CODEX_ACCESS_TOKEN.
	AuthSubscription = "subscription"
	// AuthBilling uses CODEX_API_KEY and usage-based API billing.
	AuthBilling = "billing"
)

// Config is the per-process Codex runner configuration.
type Config struct {
	Bin              string
	Sandbox          string
	ApprovalPolicy   string
	AuthMode         string
	SkipGitRepoCheck bool
	ExtraArgs        []string
}

// New returns a agent.Runner backed by `codex exec --json`.
func New(cfg Config) agent.Runner {
	if cfg.Bin == "" {
		cfg.Bin = "codex"
	}
	return &runner{cfg: cfg}
}

type runner struct {
	cfg Config
}

// killGrace is how long the child gets to exit after SIGTERM before SIGKILL.
const killGrace = 500 * time.Millisecond

// maxScanLine is the per-line buffer cap for Codex JSONL events.
const maxScanLine = 16 * 1024 * 1024

// initScanBuf is the scanner's initial buffer size.
const initScanBuf = 64 * 1024

// stderrTailBytes is how much trailing stderr we keep to enrich errors.
const stderrTailBytes = 4096

func (r *runner) Run(ctx context.Context, prompt string, o agent.Options) (<-chan agent.Event, error) {
	args := r.buildArgs(prompt, o)

	//nolint:gosec,noctx // bin+args are operator config; ctx drives process-group kill in stream.
	cmd := exec.Command(r.cfg.Bin, args...)
	cmd.Dir = o.Workdir
	if env := r.childEnv(o.Env); env != nil {
		cmd.Env = env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrTail := newTail(stderrTailBytes)
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", r.cfg.Bin, err)
	}

	out := make(chan agent.Event)
	go r.stream(ctx, cmd, stdout, stderrTail, out)
	return out, nil
}

func (r *runner) buildArgs(prompt string, o agent.Options) []string {
	if o.SessionID != "" {
		args := r.appendOptions([]string{"exec", "resume"}, o, false)
		return append(args, o.SessionID, prompt)
	}
	args := r.appendOptions([]string{"exec"}, o, true)
	return append(args, prompt)
}

func (r *runner) appendOptions(args []string, o agent.Options, includeCD bool) []string {
	args = append(args, "--json")
	if includeCD && o.Workdir != "" {
		args = append(args, "--cd", o.Workdir)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if r.cfg.Sandbox != "" {
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", r.cfg.Sandbox))
	}
	if r.cfg.ApprovalPolicy != "" {
		args = append(args, "-c", fmt.Sprintf("approval_policy=%q", r.cfg.ApprovalPolicy))
	}
	if r.cfg.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	return append(args, r.cfg.ExtraArgs...)
}

func (r *runner) childEnv(env []string) []string {
	if r.authMode() != AuthSubscription {
		if len(env) == 0 {
			return nil
		}
		return env
	}
	if len(env) == 0 {
		env = os.Environ()
	}
	return removeEnv(env, "CODEX_API_KEY")
}

func (r *runner) authMode() string {
	switch r.cfg.AuthMode {
	case "", AuthSubscription:
		return AuthSubscription
	case AuthBilling:
		return AuthBilling
	default:
		return r.cfg.AuthMode
	}
}

func removeEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (r *runner) stream(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, stderrTail *tail, out chan<- agent.Event) {
	defer close(out)

	done := make(chan struct{})
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

	sawTerminal, scanErr := r.parse(ctx, stdout, out)

	waitErr := cmd.Wait()
	killMu.Lock()
	if killTimer != nil {
		killTimer.Stop()
	}
	killMu.Unlock()

	if ctx.Err() != nil || sawTerminal {
		return
	}
	emit(ctx, out, agent.Event{Type: agent.RunError, Err: exitError(waitErr, stderrTail, scanErr)})
}

func (r *runner) parse(ctx context.Context, stdout io.Reader, out chan<- agent.Event) (bool, error) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, initScanBuf), maxScanLine)

	state := &streamState{}
	var sawTerminal bool
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		for _, e := range decode(line, state) {
			if e.Type == agent.Result || e.Type == agent.RunError {
				sawTerminal = true
			}
			if !emit(ctx, out, e) {
				return sawTerminal, sc.Err()
			}
		}
	}
	return sawTerminal, sc.Err()
}

type streamState struct {
	threadID string
	texts    []string
}

type envelope struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"` //nolint:tagliatelle // Codex emits snake_case.
	Item     json.RawMessage `json:"item"`
	Message  string          `json:"message"`
	Error    *codexError     `json:"error"`
}

type codexError struct {
	Message string `json:"message"`
}

type item struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Command string          `json:"command"`
	Content []contentBlock  `json:"content"`
	Input   json.RawMessage `json:"input"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func decode(line []byte, state *streamState) []agent.Event {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil
	}

	if env.ThreadID != "" {
		state.threadID = env.ThreadID
	}

	switch env.Type {
	case "thread.started":
		if state.threadID == "" {
			return nil
		}
		return []agent.Event{{Type: agent.SystemInit, SessionID: state.threadID}}
	case "item.started":
		it, ok := decodeItem(env.Item)
		if !ok || it.Type != "command_execution" {
			return nil
		}
		input := env.Item
		if len(input) == 0 && len(it.Input) > 0 {
			input = it.Input
		}
		return []agent.Event{{Type: agent.ToolUse, Tool: it.Type, ToolInput: input}}
	case "item.completed":
		it, ok := decodeItem(env.Item)
		if !ok {
			return nil
		}
		switch it.Type {
		case "agent_message":
			text := itemText(it)
			if text == "" {
				return nil
			}
			state.texts = append(state.texts, text)
			return []agent.Event{{Type: agent.Text, Text: text}}
		case "command_execution":
			return []agent.Event{{Type: agent.ToolResult}}
		default:
			return nil
		}
	case "turn.completed":
		return []agent.Event{{
			Type: agent.Result,
			Result: &agent.RunResult{
				Text:      joinTexts(state.texts),
				SessionID: state.threadID,
				IsError:   false,
				Subtype:   "success",
			},
		}}
	case "turn.failed", "error":
		return []agent.Event{{Type: agent.RunError, Err: errors.New(errorMessage(env))}}
	default:
		return nil
	}
}

func decodeItem(raw json.RawMessage) (item, bool) {
	if len(raw) == 0 {
		return item{}, false
	}
	var it item
	if err := json.Unmarshal(raw, &it); err != nil {
		return item{}, false
	}
	return it, true
}

func itemText(it item) string {
	if it.Text != "" {
		return it.Text
	}
	var text string
	for _, b := range it.Content {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if text != "" {
			text += "\n"
		}
		text += b.Text
	}
	return text
}

func joinTexts(texts []string) string {
	var out string
	for _, text := range texts {
		if text == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += text
	}
	return out
}

func errorMessage(env envelope) string {
	if env.Error != nil && env.Error.Message != "" {
		return "codex turn failed: " + env.Error.Message
	}
	if env.Message != "" {
		return "codex turn failed: " + env.Message
	}
	return "codex turn failed"
}

func emit(ctx context.Context, out chan<- agent.Event, e agent.Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

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

func exitError(waitErr error, stderrTail *tail, scanErr error) error {
	tail := stderrTail.String()
	var ee *exec.ExitError
	switch {
	case waitErr != nil && errors.As(waitErr, &ee):
		if tail != "" {
			return fmt.Errorf("codex exited with code %d: %s", ee.ExitCode(), tail)
		}
		return fmt.Errorf("codex exited with code %d", ee.ExitCode())
	case waitErr != nil:
		if tail != "" {
			return fmt.Errorf("codex run failed: %w: %s", waitErr, tail)
		}
		return fmt.Errorf("codex run failed: %w", waitErr)
	case scanErr != nil:
		if tail != "" {
			return fmt.Errorf("codex stream scan failed before a terminal event: %w: %s", scanErr, tail)
		}
		return fmt.Errorf("codex stream scan failed before a terminal event: %w", scanErr)
	default:
		if tail != "" {
			return fmt.Errorf("codex ended without a terminal event: %s", tail)
		}
		return errors.New("codex ended without a terminal event")
	}
}
