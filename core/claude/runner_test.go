package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBin is the canned-stream fake claude script under testdata.
const fakeBin = "testdata/fake-claude.sh"

// runStream runs the fake claude against the given canned JSONL file and
// collects every emitted event. It fails the test if the channel does not
// close within a generous deadline.
func runStream(t *testing.T, ctx context.Context, streamFile string, o Options) []Event {
	t.Helper()
	abs, err := filepath.Abs(streamFile)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	o.Env = append(os.Environ(), "FAKE_CLAUDE_STREAM="+abs)

	r := New(fakeBin)
	ch, err := r.Run(ctx, "say hi", o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var got []Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("channel did not close in time; got %d events", len(got))
		}
	}
}

func TestRun_Happy(t *testing.T) {
	events := runStream(t, context.Background(), "testdata/happy.jsonl", Options{Model: "claude-opus-4-8", MaxTurns: 10})

	gotTypes := make([]EventType, len(events))
	for i, e := range events {
		gotTypes[i] = e.Type
	}
	wantTypes := []EventType{SystemInit, Text, ToolUse, ToolResult, Text, Result}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("event[%d] type = %d, want %d (full: %v)", i, gotTypes[i], wantTypes[i], gotTypes)
		}
	}

	// SystemInit carries the session id.
	if got := events[0].SessionID; got != "0fede208-aee3-4b42-9653-5d38cad0f1ed" {
		t.Errorf("SystemInit SessionID = %q", got)
	}
	// First text block.
	if got := events[1].Text; got != "Let me check the files." {
		t.Errorf("Text = %q", got)
	}
	// Tool use: Bash with non-empty input.
	if got := events[2].Tool; got != "Bash" {
		t.Errorf("ToolUse Tool = %q, want Bash", got)
	}
	if len(events[2].ToolInput) == 0 {
		t.Error("ToolUse ToolInput is empty")
	}
	var input map[string]any
	if err := json.Unmarshal(events[2].ToolInput, &input); err != nil {
		t.Errorf("ToolInput not valid JSON: %v", err)
	} else if input["command"] != "ls -la" {
		t.Errorf("ToolInput command = %v, want 'ls -la'", input["command"])
	}

	// Terminal result.
	res := events[len(events)-1].Result
	if res == nil {
		t.Fatal("Result event has nil RunResult")
	}
	if res.SessionID != "0fede208-aee3-4b42-9653-5d38cad0f1ed" {
		t.Errorf("Result.SessionID = %q", res.SessionID)
	}
	if res.NumTurns != 3 {
		t.Errorf("Result.NumTurns = %d, want 3", res.NumTurns)
	}
	if res.CostUSD != 0.0123 {
		t.Errorf("Result.CostUSD = %v, want 0.0123", res.CostUSD)
	}
	if res.DurationMS != 4210 {
		t.Errorf("Result.DurationMS = %d, want 4210", res.DurationMS)
	}
	if res.IsError {
		t.Error("Result.IsError = true, want false")
	}
	if res.Subtype != "success" {
		t.Errorf("Result.Subtype = %q, want success", res.Subtype)
	}
	if res.Text != "The directory is empty." {
		t.Errorf("Result.Text = %q", res.Text)
	}
}

func TestRun_Error(t *testing.T) {
	events := runStream(t, context.Background(), "testdata/error.jsonl", Options{})

	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.Type != Result {
		t.Fatalf("last event type = %d, want Result", last.Type)
	}
	if last.Result == nil {
		t.Fatal("Result is nil")
	}
	if !last.Result.IsError {
		t.Error("Result.IsError = false, want true")
	}
	if last.Result.Subtype != "error_max_turns" {
		t.Errorf("Result.Subtype = %q, want error_max_turns", last.Result.Subtype)
	}
}

// TestRun_ForwardCompat verifies the decoder tolerates future additions: an
// unknown top-level event type, an unknown system subtype, and extra/unexpected
// fields on known events. The stream must still parse and yield the known events
// plus the terminal RunResult.
func TestRun_ForwardCompat(t *testing.T) {
	events := runStream(t, context.Background(), "testdata/forward_compat.jsonl", Options{})

	gotTypes := make([]EventType, len(events))
	for i, e := range events {
		gotTypes[i] = e.Type
	}
	// The unknown event type and unknown system subtype are skipped; SystemInit,
	// the assistant Text, and the Result survive.
	wantTypes := []EventType{SystemInit, Text, Result}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("event[%d] type = %d, want %d (full: %v)", i, gotTypes[i], wantTypes[i], gotTypes)
		}
	}
	if got := events[0].SessionID; got != "fc-session" {
		t.Errorf("SystemInit SessionID = %q, want fc-session", got)
	}
	if got := events[1].Text; got != "hi there" {
		t.Errorf("Text = %q, want 'hi there'", got)
	}
	res := events[len(events)-1].Result
	if res == nil {
		t.Fatal("Result event has nil RunResult")
	}
	if res.Text != "hi there" {
		t.Errorf("Result.Text = %q, want 'hi there'", res.Text)
	}
	if res.Subtype != "success" {
		t.Errorf("Result.Subtype = %q, want success", res.Subtype)
	}
}

// TestRun_NoResultCleanExit verifies that a process which emits some non-result
// events and then exits 0 WITHOUT a result envelope still yields a RunError
// ("ended without a result event").
func TestRun_NoResultCleanExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "noresult.sh")
	// Emit an init and an assistant text block, then exit 0 with no result line.
	body := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"nr-session\"}'\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"working\"}]}}'\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	r := New(script)
	ch, err := r.Run(context.Background(), "go", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var runErr error
	var sawText bool
	for e := range ch {
		switch e.Type {
		case Text:
			sawText = true
		case RunError:
			runErr = e.Err
		}
	}
	if !sawText {
		t.Error("expected the assistant Text event before the error")
	}
	if runErr == nil {
		t.Fatal("expected a RunError event for a result-less clean exit")
	}
	if !strings.Contains(runErr.Error(), "ended without a result event") {
		t.Errorf("error %q missing 'ended without a result event'", runErr.Error())
	}
}

// TestRun_Cancellation verifies that cancelling the context kills the child
// (and its process group) and closes the channel within ~1s, even when the
// fake never emits anything.
func TestRun_Cancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sleeper.sh")
	// Trap signals so a naive SIGTERM-ignore can't keep it alive; the runner
	// escalates to SIGKILL on the process group regardless.
	body := "#!/bin/sh\ntrap '' TERM\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := New(script)
	ch, err := r.Run(ctx, "hang", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	for range ch { //nolint:revive // draining the channel until it closes
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("channel took %v to close after cancel; expected ~1s", elapsed)
	}
}

// TestRun_LargeLine ensures a single stream-json line exceeding bufio's 64KB
// default parses without a "token too long" error.
func TestRun_LargeLine(t *testing.T) {
	dir := t.TempDir()
	stream := filepath.Join(dir, "big.jsonl")

	big := strings.Repeat("x", 200*1024) // 200KB text block, well over 64KB
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"big-session"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + big + `"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"big-session","num_turns":1,"total_cost_usd":0,"duration_ms":1}`,
	}
	if err := os.WriteFile(stream, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write stream: %v", err)
	}

	events := runStream(t, context.Background(), stream, Options{})

	var sawBigText, sawResult bool
	for _, e := range events {
		if e.Type == Text && len(e.Text) == len(big) {
			sawBigText = true
		}
		if e.Type == Result {
			sawResult = true
		}
	}
	if !sawBigText {
		t.Error("did not parse the >64KB text block")
	}
	if !sawResult {
		t.Error("did not reach the result event")
	}
}

// TestRun_TextArgMode asserts the DEFAULT (no images) path passes the prompt as
// the trailing CLI arg and writes NOTHING to stdin — the backward-compatible
// text path. The fake echoes its args + a stdin-byte-count into the result text.
func TestRun_TextArgMode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "argmode.sh")
	// Capture argv and the number of stdin bytes, then emit a minimal stream whose
	// result text encodes both, so the test can assert the mode from the outside.
	body := "#!/bin/sh\n" +
		"args=\"$*\"\n" +
		"n=$(cat | wc -c | tr -d ' ')\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"arg-session\"}'\n" +
		"printf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"stdin=%s args=%s\",\"session_id\":\"arg-session\",\"num_turns\":1,\"total_cost_usd\":0,\"duration_ms\":1}\\n' \"$n\" \"$args\"\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	r := New(script)
	ch, err := r.Run(context.Background(), "describe this", Options{Model: "m"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var res *RunResult
	for e := range ch {
		if e.Type == Result {
			res = e.Result
		}
	}
	if res == nil {
		t.Fatal("no result event")
	}
	if !strings.Contains(res.Text, "stdin=0") {
		t.Errorf("text path wrote to stdin; result = %q, want stdin=0", res.Text)
	}
	// The prompt must appear as a trailing arg (not --input-format stream-json).
	if !strings.Contains(res.Text, "describe this") {
		t.Errorf("prompt not passed as trailing arg; result = %q", res.Text)
	}
	if strings.Contains(res.Text, "--input-format") {
		t.Errorf("text path must not set --input-format; result = %q", res.Text)
	}
}

// TestBuildArgs_AddDirNeverSwallowsPrompt guards the regression where --add-dir,
// which is VARIADIC in the claude CLI, was the last flag emitted right before the
// trailing [prompt] positional and greedily ate the prompt as a second directory —
// leaving the CLI with no input ("Input must be provided either through stdin or
// as a prompt argument when using --print"). The prompt must be the final arg AND
// be separated from --add-dir's value by a flag, so the variadic stops before it.
func TestBuildArgs_AddDirNeverSwallowsPrompt(t *testing.T) {
	const prompt = "привет!"
	args := buildArgs(Options{Workdir: "/workspace", Model: "m", MaxTurns: 40, SessionID: "s"}, prompt, false)

	if len(args) == 0 || args[len(args)-1] != prompt {
		t.Fatalf("prompt must be the last arg; got %q", args)
	}
	idx := -1
	for i, a := range args {
		if a == "--add-dir" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("--add-dir not emitted when Workdir set; args = %q", args)
	}
	if idx+2 >= len(args) {
		t.Fatalf("--add-dir is at the tail with nothing after its value; args = %q", args)
	}
	if got := args[idx+1]; got != "/workspace" {
		t.Fatalf("--add-dir value = %q, want /workspace; args = %q", got, args)
	}
	// The token right after the --add-dir value must be a flag (variadic stop), and
	// in particular must NOT be the prompt.
	if next := args[idx+2]; !strings.HasPrefix(next, "--") || next == prompt {
		t.Fatalf("token after --add-dir value must be a flag, got %q; args = %q", next, args)
	}
}

// TestRun_ImageStdinMode asserts that a run carrying an image switches to
// stream-json stdin input: --input-format stream-json is set, the prompt is NOT a
// trailing arg, and a valid user-message envelope (text + base64 image block) is
// written to the child's stdin. The fake validates the envelope shape and decodes
// the image bytes back, echoing the outcome into the result so the test can assert
// the stream still parses.
func TestRun_ImageStdinMode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "imgmode.sh")
	captured := filepath.Join(dir, "stdin.json")
	argsFile := filepath.Join(dir, "args.txt")
	// Persist argv + stdin so the test can inspect the exact envelope, then emit a
	// canned successful stream.
	body := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + argsFile + "\n" +
		"cat > " + captured + "\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"img-session\"}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"session_id\":\"img-session\",\"num_turns\":1,\"total_cost_usd\":0,\"duration_ms\":1}'\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a} // arbitrary "PNG-ish" bytes
	r := New(script)
	ch, err := r.Run(context.Background(), "what is in this image?", Options{
		Images: []ImageInput{{MediaType: "image/png", Data: imgBytes}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var res *RunResult
	for e := range ch {
		if e.Type == Result {
			res = e.Result
		}
	}
	if res == nil || res.Text != "ok" {
		t.Fatalf("image-mode stream did not parse to a result; res=%+v", res)
	}

	// Args: --input-format stream-json present, prompt NOT a trailing arg.
	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsRaw)
	if !strings.Contains(args, "--input-format stream-json") {
		t.Errorf("image mode missing --input-format stream-json; args = %q", args)
	}
	if strings.Contains(args, "what is in this image?") {
		t.Errorf("image mode must not pass the prompt as a trailing arg; args = %q", args)
	}

	// Stdin: a valid user-message envelope with a text block and a base64 image
	// block whose decoded bytes round-trip to the original image.
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("stdin envelope not valid JSON: %v\nraw=%q", err, raw)
	}
	if env.Type != "user" || env.Message.Role != "user" {
		t.Errorf("envelope type/role = %q/%q, want user/user", env.Type, env.Message.Role)
	}
	if len(env.Message.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (text + image)", len(env.Message.Content))
	}
	if env.Message.Content[0].Type != "text" || env.Message.Content[0].Text != "what is in this image?" {
		t.Errorf("text block = %+v, want type=text text=prompt", env.Message.Content[0])
	}
	img := env.Message.Content[1]
	if img.Type != "image" || img.Source == nil {
		t.Fatalf("image block = %+v, want type=image with source", img)
	}
	if img.Source.Type != "base64" || img.Source.MediaType != "image/png" {
		t.Errorf("image source = %+v, want base64/image/png", img.Source)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Source.Data)
	if err != nil {
		t.Fatalf("image data not valid base64: %v", err)
	}
	if !bytes.Equal(decoded, imgBytes) {
		t.Errorf("decoded image = %v, want %v", decoded, imgBytes)
	}
}

// TestRun_NoResultNonZeroExit verifies that a process which exits non-zero
// without a result envelope yields a RunError carrying the exit code and the
// stderr tail.
func TestRun_NoResultNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "boom.sh")
	body := "#!/bin/sh\necho 'fatal: kaboom' >&2\nexit 7\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	r := New(script)
	ch, err := r.Run(context.Background(), "go", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var runErr error
	for e := range ch {
		if e.Type == RunError {
			runErr = e.Err
		}
	}
	if runErr == nil {
		t.Fatal("expected a RunError event")
	}
	if !strings.Contains(runErr.Error(), "7") {
		t.Errorf("error %q missing exit code 7", runErr.Error())
	}
	if !strings.Contains(runErr.Error(), "kaboom") {
		t.Errorf("error %q missing stderr tail", runErr.Error())
	}
}
