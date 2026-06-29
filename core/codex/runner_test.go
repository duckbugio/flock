//nolint:testpackage // intentionally whitebox to test runner internals and args
package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/agent"
)

func writeFakeCodex(t *testing.T, dir string) (script, argsFile, envFile string) {
	t.Helper()
	script = filepath.Join(dir, "fake-codex.sh")
	argsFile = filepath.Join(dir, "args.txt")
	envFile = filepath.Join(dir, "env.txt")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > " + argsFile + "\n" +
		"printf 'CODEX_HOME=%s\\nCODEX_API_KEY=%s\\nCODEX_ACCESS_TOKEN=%s\\n' \"$CODEX_HOME\" \"$CODEX_API_KEY\" \"$CODEX_ACCESS_TOKEN\" > " + envFile + "\n" +
		"cat \"$FAKE_CODEX_STREAM\"\n"
	//nolint:gosec // test fixture script permissions
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return script, argsFile, envFile
}

func writeStream(t *testing.T, dir string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	return path
}

func collect(t *testing.T, ch <-chan agent.Event) []agent.Event {
	t.Helper()
	var events []agent.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatalf("runner channel did not close; got %d events", len(events))
		}
	}
}

func TestRunHappyMapsCodexJSONLToClaudeEvents(t *testing.T) {
	dir := t.TempDir()
	bin, argsFile, envFile := writeFakeCodex(t, dir)
	stream := writeStream(t, dir, []string{
		`{"type":"thread.started","thread_id":"thr_123"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"I checked the repo."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}`,
	})
	env := append(os.Environ(),
		"FAKE_CODEX_STREAM="+stream,
		"CODEX_HOME="+filepath.Join(dir, "codex-home"),
		"CODEX_API_KEY=sk-should-not-leak",
	)

	r := New(Config{Bin: bin, Sandbox: "workspace-write", ApprovalPolicy: "never", AuthMode: AuthSubscription})
	ch, err := r.Run(context.Background(), "summarize", agent.Options{
		Workdir: dir,
		Model:   "gpt-5.5",
		Env:     env,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := collect(t, ch)

	gotTypes := make([]agent.EventType, len(events))
	for i, ev := range events {
		gotTypes[i] = ev.Type
	}
	wantTypes := []agent.EventType{agent.SystemInit, agent.ToolUse, agent.Text, agent.Result}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("event %d type = %v, want %v; all=%v", i, gotTypes[i], wantTypes[i], gotTypes)
		}
	}
	if events[0].SessionID != "thr_123" {
		t.Errorf("SystemInit.SessionID = %q, want thr_123", events[0].SessionID)
	}
	if events[1].Tool != "command_execution" {
		t.Errorf("ToolUse.Tool = %q, want command_execution", events[1].Tool)
	}
	var input map[string]any
	if err := json.Unmarshal(events[1].ToolInput, &input); err != nil {
		t.Fatalf("ToolInput invalid JSON: %v", err)
	}
	if input["command"] != "bash -lc ls" {
		t.Errorf("ToolInput command = %v, want bash -lc ls", input["command"])
	}
	if events[2].Text != "I checked the repo." {
		t.Errorf("Text = %q, want final agent text", events[2].Text)
	}
	if events[3].Result == nil || events[3].Result.SessionID != "thr_123" || events[3].Result.Text != "I checked the repo." {
		t.Fatalf("Result = %+v, want session thr_123 and final text", events[3].Result)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsRaw)
	for _, want := range []string{
		"exec --json",
		"--cd " + dir,
		"--model gpt-5.5",
		"-c sandbox_mode=\"workspace-write\"",
		"-c approval_policy=\"never\"",
		"summarize",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	envRaw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if strings.Contains(string(envRaw), "CODEX_API_KEY=sk-") {
		t.Fatalf("subscription run leaked API key env: %s", envRaw)
	}
}

func TestRunResumeUsesCodexExecResume(t *testing.T) {
	dir := t.TempDir()
	bin, argsFile, _ := writeFakeCodex(t, dir)
	stream := writeStream(t, dir, []string{
		`{"type":"thread.started","thread_id":"thr_existing"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"continued"}}`,
		`{"type":"turn.completed"}`,
	})
	env := append(os.Environ(), "FAKE_CODEX_STREAM="+stream)

	r := New(Config{Bin: bin, Sandbox: "read-only", ApprovalPolicy: "never", AuthMode: AuthSubscription})
	ch, err := r.Run(context.Background(), "continue", agent.Options{
		SessionID: "thr_existing",
		Workdir:   dir,
		Env:       env,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = collect(t, ch)

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsRaw)
	if !strings.Contains(args, "exec resume --json") ||
		!strings.Contains(args, "thr_existing continue") ||
		strings.Contains(args, "--cd ") {
		t.Fatalf("resume args = %q, want exec resume options before session and no --cd", args)
	}
}

func TestRunErrorWhenCodexExitsWithoutTurnCompleted(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "boom.sh")
	body := "#!/bin/sh\necho 'codex failed' >&2\nexit 7\n"
	//nolint:gosec // test fixture script permissions
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	r := New(Config{Bin: script, AuthMode: AuthSubscription})
	ch, err := r.Run(context.Background(), "go", agent.Options{Workdir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var runErr error
	for _, ev := range collect(t, ch) {
		if ev.Type == agent.RunError {
			runErr = ev.Err
		}
	}
	if runErr == nil {
		t.Fatal("expected RunError")
	}
	if !strings.Contains(runErr.Error(), "7") || !strings.Contains(runErr.Error(), "codex failed") {
		t.Fatalf("RunError = %q, want exit code and stderr tail", runErr.Error())
	}
}
