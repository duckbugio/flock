package claude

import (
	"encoding/json"
	"fmt"
	"os"
)

// Context7URL is the context7 MCP HTTP endpoint. context7 serves up-to-date,
// version-specific library/API documentation as tools (resolve-library-id,
// get-library-docs), so the agent looks real docs up instead of guessing from
// stale training data — the single highest-leverage accuracy boost for a coding
// agent. The free tier needs no auth; an unreachable endpoint never breaks a run
// (the CLI just runs without these tools).
const Context7URL = "https://mcp.context7.com/mcp"

// mcpConfigPerm is the owner-only permission for the written MCP config file —
// it may carry a bearer token, so it must never be group/world readable.
const mcpConfigPerm os.FileMode = 0o600

// MCPServer is one streamable-HTTP entry of the .mcp.json mcpServers map.
type MCPServer struct {
	// URL is the server's HTTP endpoint.
	URL string
	// BearerToken, when non-empty, is sent as "Authorization: Bearer <token>".
	BearerToken string
}

// WriteMCPConfig writes a Claude Code .mcp.json enabling the given servers to
// path (overwriting any existing file). It reports ok=false — writing nothing —
// when servers is empty, so callers wire Options.MCPConfig only when at least
// one server is actually enabled. The config matches the Claude Code .mcp.json
// schema:
//
//	{"mcpServers":{"<name>":{"type":"http","url":"…","headers":{"Authorization":"Bearer …"}}}}
func WriteMCPConfig(path string, servers map[string]MCPServer) (bool, error) {
	if len(servers) == 0 {
		return false, nil
	}
	entries := make(map[string]any, len(servers))
	for name, s := range servers {
		entry := map[string]any{
			"type": "http",
			"url":  s.URL,
		}
		if s.BearerToken != "" {
			entry["headers"] = map[string]string{"Authorization": "Bearer " + s.BearerToken}
		}
		entries[name] = entry
	}
	cfg := map[string]any{"mcpServers": entries}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(path, data, mcpConfigPerm); err != nil {
		return false, fmt.Errorf("write mcp config %s: %w", path, err)
	}
	return true, nil
}
