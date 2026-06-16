package claude_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/claude"
)

// TestWriteContext7MCPConfig asserts the written file is valid JSON matching the
// Claude Code .mcp.json schema with a context7 HTTP docs server.
func TestWriteContext7MCPConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := claude.WriteContext7MCPConfig(path); err != nil {
		t.Fatalf("WriteContext7MCPConfig: %v", err)
	}
	//nolint:gosec // G304: path is a test-controlled temp file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	// Decode into maps (not a tagged struct) so the test carries no snake/camel json
	// tags of its own — it just checks the on-disk shape.
	var cfg map[string]map[string]map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config is not valid JSON: %v\n%s", err, data)
	}
	c7, ok := cfg["mcpServers"]["context7"]
	if !ok {
		t.Fatalf("context7 server missing from config: %s", data)
	}
	if c7["type"] != "http" {
		t.Errorf("context7 type = %v, want http", c7["type"])
	}
	url, _ := c7["url"].(string)
	if !strings.HasPrefix(url, "https://") || !strings.Contains(url, "context7") {
		t.Errorf("context7 url = %q, want an https context7 endpoint", url)
	}
}
