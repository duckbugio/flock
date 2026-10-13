package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/duckbugio/flock/core/knowledge"
)

const delegatedDirPerm os.FileMode = 0o700

func prepareDelegated(ctx context.Context, o Options) (Options, func(), error) {
	if o.AnswerOnly {
		return o, nil, errors.New("incompatible delegated run policies")
	}
	base := o.Env
	if len(base) == 0 {
		base = os.Environ()
	}
	o.Env = delegatedEnv(base)
	var authenticated bool
	for _, kv := range o.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") {
			_, value, _ := strings.Cut(kv, "=")
			authenticated = authenticated || strings.TrimSpace(value) != ""
		}
	}
	if !authenticated {
		return o, nil, errors.New("delegated Claude requires explicit provider authentication")
	}
	instructions, err := os.ReadFile(filepath.Join(o.Workdir, "CLAUDE.md"))
	if err != nil || len(instructions) > 1<<20 {
		return o, nil, errors.New("delegated instructions unavailable")
	}
	servers, closeKnowledge, err := delegatedServers(ctx, o.MCPConfig)
	if err != nil {
		return o, nil, err
	}
	dir, err := os.MkdirTemp("", "flock-secretary-")
	if err != nil {
		closeKnowledge()
		return o, nil, errors.New("delegated workspace unavailable")
	}
	cleanup := func() {
		closeKnowledge()
		_ = os.RemoveAll(dir)
	}
	// The adapter's readonly_v1 workspace is scoped to one owner and recipient.
	// LO additionally separates connection and policy generations.
	// Preserve only its isolated CLI history; never resume ordinary agent sessions.
	configDir := filepath.Join(o.Workdir, ".secretary-config")
	if err := os.Mkdir(configDir, delegatedDirPerm); err != nil {
		info, statErr := os.Lstat(configDir)
		if !os.IsExist(err) || statErr != nil || !info.IsDir() || info.Mode().Perm() != delegatedDirPerm {
			cleanup()
			return o, nil, errors.New("delegated configuration unavailable")
		}
	}
	//nolint:gosec // private MkdirTemp directory and fixed filename; no untrusted path component.
	if err := os.WriteFile(filepath.Join(dir, "instructions.txt"), instructions, mcpConfigPerm); err != nil {
		cleanup()
		return o, nil, errors.New("delegated instructions unavailable")
	}
	// An explicit empty MCP configuration prevents inherited project/user servers.
	configPath := filepath.Join(dir, "mcp.json")
	if len(servers) == 0 {
		err = os.WriteFile(configPath, []byte(`{"mcpServers":{}}`), mcpConfigPerm)
	} else {
		_, err = WriteMCPConfig(configPath, servers)
	}
	if err != nil {
		cleanup()
		return o, nil, errors.New("delegated MCP configuration unavailable")
	}
	o.Workdir, o.MCPConfig = dir, configPath
	o.Env = append(o.Env, "CLAUDE_CONFIG_DIR="+configDir, "CLAUDE_CODE_PROJECT_DIR_NAME=secretary",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	return o, cleanup, nil
}

func delegatedEnv(env []string) []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TMPDIR": true,
		"TMP": true, "TEMP": true, "SYSTEMROOT": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_BASE_URL": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	}
	out := make([]string, 0, len(allowed))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && allowed[key] {
			out = append(out, kv)
		}
	}
	return out
}

func delegatedServers(ctx context.Context, path string) (map[string]MCPServer, func(), error) {
	servers := map[string]MCPServer{}
	cleanup := func() {}
	if path == "" {
		return servers, cleanup, nil
	}
	//nolint:gosec // caller-owned host MCP config, never a model or incoming-message path.
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return nil, nil, errors.New("delegated MCP configuration unavailable")
	}
	var config struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, nil, errors.New("invalid delegated MCP configuration")
	}
	upstream, ok := config.Servers["duckbug"]
	if !ok {
		return servers, cleanup, nil
	}
	if upstream.Type != "http" {
		return nil, nil, errors.New("unsupported delegated MCP transport")
	}
	proxy, err := knowledge.Start(ctx, upstream.URL, upstream.Headers["Authorization"])
	if err != nil {
		return nil, nil, errors.New("delegated knowledge connection unavailable")
	}
	servers["secretary_knowledge"] = MCPServer{URL: proxy.URL, BearerToken: proxy.Token}
	return servers, proxy.Close, nil
}
