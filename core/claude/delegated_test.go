//nolint:testpackage // whitebox coverage of the delegated launch boundary.
package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDelegatedLaunchExcludesCredentialsAndOrdinaryWorkspace(t *testing.T) {
	ownerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ownerDir, "CLAUDE.md"), []byte("Owner disclosure rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := Options{
		DelegatedReadOnly: true, Workdir: ownerDir, SessionID: "readonly-session", Effort: "ultracode",
		Env: []string{
			"PATH=/bin", "HOME=" + t.TempDir(), "ANTHROPIC_API_KEY=provider-fixture",
			"LO_BOT_TOKEN=private-bot", "DUCKBUG_TOKEN=private-mcp", "GITHUB_TOKEN=private-git", "DOCKER_HOST=private-docker",
			"NODE_OPTIONS=--require=unsafe", "CLAUDE_CONFIG_DIR=/ordinary/session", "CLAUDE_CODE_PROJECT_DIR_NAME=ordinary",
		},
	}
	prepared, cleanup, err := prepareDelegated(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if prepared.Workdir == ownerDir || prepared.SessionID != input.SessionID {
		t.Fatal("ordinary workspace inherited or scoped session lost")
	}
	for _, entry := range prepared.Env {
		for _, forbidden := range []string{
			"private-bot", "private-mcp", "private-git", "private-docker",
			"unsafe", "/ordinary/session",
		} {
			if strings.Contains(entry, forbidden) {
				t.Fatal("untrusted credential/configuration inherited")
			}
		}
	}
	args := buildArgs(prepared, "incoming", false)
	for _, flag := range []string{
		"--restricted", "--tools", "--strict-mcp-config",
		"--setting-sources", "--disable-slash-commands",
	} {
		if !slices.Contains(args, flag) {
			t.Fatalf("missing boundary %s", flag)
		}
	}
	if slices.Contains(args, "bypassPermissions") || slices.Contains(args, "--add-dir") {
		t.Fatal("execution permission or ordinary workspace exposed")
	}
	settingsIndex := slices.Index(args, "--settings")
	var settings struct {
		DisableAllHooks   bool `json:"disableAllHooks"`
		AutoMemoryEnabled bool `json:"autoMemoryEnabled"`
		Ultracode         bool `json:"ultracode"`
	}
	if settingsIndex < 0 || json.Unmarshal([]byte(args[settingsIndex+1]), &settings) != nil ||
		!settings.DisableAllHooks || settings.AutoMemoryEnabled || !settings.Ultracode {
		t.Fatal("effort weakened delegated configuration")
	}
	config, err := os.ReadFile(prepared.MCPConfig)
	if err != nil || string(config) != `{"mcpServers":{}}` {
		t.Fatal("empty scope inherited other MCP servers")
	}
	instructions, err := os.ReadFile(filepath.Join(prepared.Workdir, "instructions.txt"))
	if err != nil || !strings.Contains(string(instructions), "no authorized knowledge-base tools are available") ||
		!strings.Contains(string(instructions), "Owner disclosure rules") {
		t.Fatal("unavailable knowledge or owner policy was not communicated")
	}
	cleanup()
	if _, err := os.Stat(prepared.Workdir); !os.IsNotExist(err) {
		t.Fatal("temporary run files retained")
	}
	if _, err := os.Stat(filepath.Join(ownerDir, ".secretary-config")); err != nil {
		t.Fatal("private recipient history removed")
	}
}

func TestDelegatedLaunchFailsClosedWithoutExplicitAuthentication(t *testing.T) {
	_, cleanup, err := prepareDelegated(t.Context(), Options{DelegatedReadOnly: true, Env: []string{"HOME=" + t.TempDir()}})
	if err == nil || cleanup != nil {
		t.Fatal("implicit host authentication was permitted")
	}
	_, cleanup, err = prepareDelegated(t.Context(), Options{DelegatedReadOnly: true, AnswerOnly: true})
	if err == nil || cleanup != nil {
		t.Fatal("conflicting execution policies were permitted")
	}
}

func TestDelegatedLaunchExplainsEmptyAuthorizedKnowledgeCatalog(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "discover_tools", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"tools": []any{}}}, nil
		})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(upstream.Close)
	owner := t.TempDir()
	if err := os.WriteFile(filepath.Join(owner, "CLAUDE.md"), []byte("Owner disclosure rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(owner, "mcp.json")
	if _, err := WriteMCPConfig(config, map[string]MCPServer{"duckbug": {URL: upstream.URL, BearerToken: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	prepared, cleanup, err := prepareDelegated(t.Context(), Options{
		Workdir: owner, MCPConfig: config,
		Env: []string{"ANTHROPIC_API_KEY=synthetic-provider"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	instructions, err := os.ReadFile(filepath.Join(prepared.Workdir, "instructions.txt"))
	if err != nil || !strings.Contains(string(instructions), "no authorized knowledge-base tools are available") {
		t.Fatal("empty discovery was hidden from the delegated agent")
	}
	data, err := os.ReadFile(prepared.MCPConfig)
	if err != nil || string(data) != `{"mcpServers":{}}` {
		t.Fatal("empty discovery inherited other MCP access")
	}
}
