//go:build integration

//nolint:testpackage // exercise the real CLI through the private launch boundary.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// No real provider/account credential is used. The pinned CLI talks only to
// local synthetic model/MCP servers; forced tool calls test actual enforcement.
const fixtureToolUse = "tool_use"

func TestIntegration_DelegatedCLIReadOnlyAndScopedResume(t *testing.T) {
	bin := os.Getenv("FLOCK_DELEGATED_CLAUDE_BIN")
	if bin == "" {
		t.Skip("set FLOCK_DELEGATED_CLAUDE_BIN to the pinned Claude CLI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var mu sync.Mutex
	reads, turns, deliveries := 0, 0, 0
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		deliveries++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer botAPI.Close()
	var inventories [][]string
	var requests []string
	upstream := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	upstream.AddTool(&mcp.Tool{Name: "discover_tools", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{StructuredContent: map[string]any{"tools": []any{map[string]any{
				"name": args["name"], "mutates": false, "execution": "read", "surface": "catalog",
				"inputSchema": map[string]any{
					"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
					"required": []string{"query"}, "additionalProperties": false,
				},
			}}}}, nil
		})
	upstream.AddTool(&mcp.Tool{Name: "execute_tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			mu.Lock()
			reads++
			mu.Unlock()
			return &mcp.CallToolResult{StructuredContent: map[string]any{
				"name":   args["name"],
				"result": map[string]any{"answer": "recipient-safe fixture"},
			}}, nil
		})
	mcpHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer mcpHTTP.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/messages") {
			http.NotFound(w, req)
			return
		}
		var body struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 2<<20))
		if err != nil || json.Unmarshal(data, &body) != nil {
			http.Error(w, "fixture request invalid", http.StatusBadRequest)
			return
		}
		mu.Lock()
		step := turns
		turns++
		requests = append(requests, string(data))
		var names []string
		for _, tool := range body.Tools {
			names = append(names, tool.Name)
		}
		inventories = append(inventories, names)
		mu.Unlock()
		block := map[string]any{"type": "text", "text": "fixture complete"}
		if step < 4 {
			name := []string{
				"mcp__secretary_knowledge__kb_search", "Bash",
				"mcp__secretary_knowledge__execute_write_tool", "mcp__lo__sendMessage",
			}[step]
			block = map[string]any{
				"type": fixtureToolUse, "id": fmt.Sprintf("fixture_%d", step), "name": name,
				"input": map[string]any{
					"query": "recipient-safe fixture", "command": "curl " + botAPI.URL + "/sendMessage",
					"url": botAPI.URL + "/sendMessage",
				},
			}
			if step == 0 {
				block["input"] = map[string]any{"query": "recipient-safe fixture"}
			}
		}
		writeModelFixture(t, w, block)
	}))
	defer model.Close()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "CLAUDE.md"),
		[]byte("Read authorized knowledge only; return a reply."), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, "mcp.json")
	if _, err := WriteMCPConfig(config, map[string]MCPServer{"duckbug": {URL: mcpHTTP.URL, BearerToken: "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	options := Options{
		DelegatedReadOnly: true, Workdir: workspace, MCPConfig: config, Model: "claude-sonnet-4-6",
		MaxTurns: 10, Effort: "ultracode", Env: []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
			"ANTHROPIC_API_KEY=synthetic-provider", "ANTHROPIC_BASE_URL=" + model.URL,
			"LO_BOT_TOKEN=must-not-reach-model", "DUCKBUG_TOKEN=must-not-reach-model",
		},
	}
	run := func(prompt string, opts Options) *RunResult {
		t.Helper()
		events, err := New(bin).Run(ctx, prompt, opts)
		if err != nil {
			t.Fatal(err)
		}
		var result *RunResult
		for event := range events {
			if event.Type == RunError {
				t.Fatalf("CLI fixture failed: %v", event.Err)
			}
			if event.Type == Result {
				result = event.Result
			}
		}
		if result == nil || result.IsError {
			t.Fatalf("CLI did not complete: %+v", result)
		}
		return result
	}
	first := run("recipient-A-marker", options)
	options.SessionID = first.SessionID
	run("continue this conversation", options)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "CLAUDE.md"), []byte("Read authorized knowledge only."), 0o600); err != nil {
		t.Fatal(err)
	}
	options.Workdir, options.SessionID = other, ""
	run("recipient-B-marker", options)
	mu.Lock()
	defer mu.Unlock()
	if reads != 1 || deliveries != 0 || len(requests) < 7 {
		t.Fatalf("read/resume requests = %d/%d", reads, len(requests))
	}
	assertDelegatedInventory(t, inventories)
	if !strings.Contains(requests[len(requests)-2], "recipient-A-marker") ||
		strings.Contains(requests[len(requests)-1], "recipient-A-marker") {
		t.Fatal("recipient history lost or crossed session scope")
	}
	for _, request := range requests {
		if strings.Contains(request, "must-not-reach-model") {
			t.Fatal("service credential reached model")
		}
	}
}

func writeModelFixture(t *testing.T, w http.ResponseWriter, block map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(kind string, payload map[string]any) {
		payload["type"] = kind
		data, err := json.Marshal(payload)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	emit("message_start", map[string]any{"message": map[string]any{
		"id": "fixture-message", "type": "message", "role": "assistant",
		"content": []any{}, "model": "claude-sonnet-4-6", "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
	}})
	start := map[string]any{"type": block["type"]}
	delta := map[string]any{}
	stop := "end_turn"
	if block["type"] == fixtureToolUse {
		start["id"], start["name"], start["input"] = block["id"], block["name"], map[string]any{}
		input, err := json.Marshal(block["input"])
		if err != nil {
			t.Error(err)
			return
		}
		delta["type"], delta["partial_json"] = "input_json_delta", string(input)
		stop = fixtureToolUse
	} else {
		start["text"] = ""
		delta["type"], delta["text"] = "text_delta", block["text"]
	}
	emit("content_block_start", map[string]any{"index": 0, "content_block": start})
	emit("content_block_delta", map[string]any{"index": 0, "delta": delta})
	emit("content_block_stop", map[string]any{"index": 0})
	emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 1},
	})
	emit("message_stop", map[string]any{})
}

func assertDelegatedInventory(t *testing.T, inventories [][]string) {
	t.Helper()
	for _, names := range inventories {
		if len(names) != 3 {
			t.Fatalf("actual CLI inventory = %v", names)
		}
		for _, name := range names {
			if name != "mcp__secretary_knowledge__kb_search" &&
				name != "mcp__secretary_knowledge__kb_get" && name != "mcp__secretary_knowledge__kb_list" {
				t.Fatalf("unexpected actual CLI tool %s", name)
			}
		}
	}
}
