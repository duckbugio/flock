package claude

import (
	"encoding/json"
	"fmt"
	"os"
)

// context7URL is the context7 MCP HTTP endpoint. context7 serves up-to-date,
// version-specific library/API documentation as tools (resolve-library-id,
// get-library-docs), so the agent looks real docs up instead of guessing from
// stale training data — the single highest-leverage accuracy boost for a coding
// agent. The free tier needs no auth; an unreachable endpoint never breaks a run
// (the CLI just runs without these tools).
const context7URL = "https://mcp.context7.com/mcp"

// mcpConfigPerm is the owner-only permission for the written MCP config file.
const mcpConfigPerm os.FileMode = 0o600

// WriteContext7MCPConfig writes an MCP servers config enabling the context7 docs
// server to path (overwriting any existing file) and returns an error only on a
// write/marshal failure. The path is then passed to each run as Options.MCPConfig
// (--mcp-config). The config matches the Claude Code .mcp.json schema:
//
//	{"mcpServers":{"context7":{"type":"http","url":"https://mcp.context7.com/mcp"}}}
func WriteContext7MCPConfig(path string) error {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"context7": map[string]any{
				"type": "http",
				"url":  context7URL,
			},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(path, data, mcpConfigPerm); err != nil {
		return fmt.Errorf("write mcp config %s: %w", path, err)
	}
	return nil
}
