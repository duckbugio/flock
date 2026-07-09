package claude_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/claude"
)

// TestWriteMCPConfig asserts the written file is valid JSON in the Claude Code
// .mcp.json schema, carrying each enabled server and its bearer header.
func TestWriteMCPConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".flock-mcp.json")
	ok, err := claude.WriteMCPConfig(path, map[string]claude.MCPServer{
		"context7": {URL: claude.Context7URL},
		//nolint:gosec // test fixture, not a credential
		"duckbug": {URL: "https://duckbug.io/api/mcp", BearerToken: "sk-duck-api01-secret"},
	})
	if err != nil || !ok {
		t.Fatalf("WriteMCPConfig = %v, %v", ok, err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config is not valid JSON: %v", err)
	}
	c7 := cfg.Servers["context7"]
	if c7.Type != "http" || c7.URL != claude.Context7URL || len(c7.Headers) != 0 {
		t.Fatalf("context7 entry = %+v", c7)
	}
	db := cfg.Servers["duckbug"]
	if db.URL != "https://duckbug.io/api/mcp" || db.Headers["Authorization"] != "Bearer sk-duck-api01-secret" {
		t.Fatalf("duckbug entry = %+v", db)
	}
	// The file may carry a token — owner-only permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", info.Mode().Perm())
	}
}

// TestWriteMCPConfigEmptyWritesNothing asserts no servers → no file, ok=false.
func TestWriteMCPConfigEmptyWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".flock-mcp.json")
	ok, err := claude.WriteMCPConfig(path, nil)
	if err != nil || ok {
		t.Fatalf("WriteMCPConfig(empty) = %v, %v; want false, nil", ok, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no file must be written for an empty server set")
	}
}

// TestWriteMCPConfigTokenNotLoggedShape sanity-checks the header value shape so
// a future refactor cannot silently drop the Bearer prefix.
func TestWriteMCPConfigTokenNotLoggedShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".flock-mcp.json")
	if _, err := claude.WriteMCPConfig(path, map[string]claude.MCPServer{
		"duckbug": {URL: "https://example.test/api/mcp", BearerToken: "tok"},
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path) //nolint:gosec // test path under t.TempDir
	if !strings.Contains(string(data), `"Bearer tok"`) {
		t.Fatalf("Authorization header malformed:\n%s", data)
	}
}
