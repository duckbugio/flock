//nolint:testpackage // whitebox tests exercise registry construction directly.
package airunner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/agent"

	"github.com/duckbugio/flock/internal/config"
)

func TestBuildDefaultProviderIsClaude(t *testing.T) {
	runner, opts, info, err := Build(config.Config{
		ClaudeBin:      "claude",
		ClaudeModel:    "claude-opus-4-8",
		ClaudeMaxTurns: 40,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runner == nil {
		t.Fatal("Build() runner = nil")
	}
	if info.Name != config.AIBackendClaude {
		t.Fatalf("provider name = %q, want claude", info.Name)
	}
	if info.DisplayName == "" {
		t.Fatal("provider display name is empty")
	}
	if !info.Capabilities.ResumeSessions || !info.Capabilities.ToolEvents || !info.Capabilities.Images {
		t.Fatalf("claude capabilities = %+v, want resume/tool/image support", info.Capabilities)
	}
	if opts.Model != "claude-opus-4-8" || opts.MaxTurns != 40 {
		t.Fatalf("opts = %+v, want claude model/max turns", opts)
	}
}

func TestBuildCodexProviderFromBackendEnv(t *testing.T) {
	runner, opts, info, err := Build(config.Config{
		AIBackend:           config.AIBackendCodex,
		CodexBin:            "codex",
		CodexModel:          "gpt-5.5",
		CodexAuthMode:       config.CodexAuthSubscription,
		CodexRequireAuth:    false,
		CodexSandbox:        "workspace-write",
		CodexApprovalPolicy: "never",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runner == nil {
		t.Fatal("Build() runner = nil")
	}
	if info.Name != config.AIBackendCodex {
		t.Fatalf("provider name = %q, want codex", info.Name)
	}
	if !info.Capabilities.ResumeSessions || !info.Capabilities.SubscriptionAuth || !info.Capabilities.APIKeyBilling {
		t.Fatalf("codex capabilities = %+v, want resume/subscription/billing support", info.Capabilities)
	}
	if !info.Capabilities.SupportsSandbox || !info.Capabilities.SupportsApprovalMode {
		t.Fatalf("codex capabilities = %+v, want sandbox/approval support", info.Capabilities)
	}
	if opts.Model != "gpt-5.5" {
		t.Fatalf("opts.Model = %q, want gpt-5.5", opts.Model)
	}
}

func TestBuildProviderAliases(t *testing.T) {
	tests := []struct {
		backend string
		want    string
	}{
		{"claude-cli", config.AIBackendClaude},
		{"codex-cli", config.AIBackendCodex},
		{"openai", config.AIBackendOpenAICompat},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			_, _, info, err := Build(config.Config{
				AIBackend:              tt.backend,
				ClaudeBin:              "claude",
				CodexBin:               "codex",
				CodexAuthMode:          config.CodexAuthSubscription,
				CodexRequireAuth:       false,
				OpenAICompatBaseURL:    "https://example.test/v1",
				OpenAICompatModel:      "qwen-plus",
				OpenAICompatAuthMode:   config.OpenAICompatAuthAPIKey,
				OpenAICompatAPIKey:     "sk-test",
				OpenAICompatBillingAck: true,
			})
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if info.Name != tt.want {
				t.Fatalf("provider name = %q, want %q", info.Name, tt.want)
			}
		})
	}
}

func TestBuildRejectsUnknownProvider(t *testing.T) {
	runner, opts, info, err := Build(config.Config{AIBackend: "qwen"})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("Build() error = %v, want ErrUnknownProvider", err)
	}
	if runner != nil || opts.Model != "" || info.Name != "" {
		t.Fatalf("Build() partial result = %T/%+v/%+v, want empty on error", runner, opts, info)
	}
}

func TestBuildOpenAICompatibleProvider(t *testing.T) {
	runner, opts, info, err := Build(config.Config{
		AIBackend:                  config.AIBackendOpenAICompat,
		OpenAICompatBaseURL:        "https://example.test/v1",
		OpenAICompatModel:          "qwen-plus",
		OpenAICompatAuthMode:       config.OpenAICompatAuthAPIKey,
		OpenAICompatAPIKey:         "sk-test",
		OpenAICompatBillingAck:     true,
		OpenAICompatTimeoutSeconds: 300,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runner == nil {
		t.Fatal("Build() runner = nil")
	}
	if info.Name != config.AIBackendOpenAICompat {
		t.Fatalf("provider name = %q, want openai-compatible", info.Name)
	}
	if info.Capabilities.ResumeSessions || info.Capabilities.ToolEvents || info.Capabilities.FileEditing {
		t.Fatalf("openai-compatible capabilities = %+v, want answer-only provider", info.Capabilities)
	}
	if !info.Capabilities.StreamEvents || !info.Capabilities.APIKeyBilling {
		t.Fatalf("openai-compatible capabilities = %+v, want stream/billing support", info.Capabilities)
	}
	if opts.Model != "qwen-plus" {
		t.Fatalf("opts.Model = %q, want qwen-plus", opts.Model)
	}
}

// TestBuildWithPendingLoginStartsUnauthorized: a Codex subscription deploy that
// has never been signed in must still BOOT, flagged pendingLogin, because the
// /login command that performs the sign-in only exists inside the running bot.
// Failing startup here is what made headless Codex setup impossible.
func TestBuildWithPendingLoginStartsUnauthorized(t *testing.T) {
	cfg := config.Config{
		AIBackend:        config.AIBackendCodex,
		CodexBin:         "codex",
		CodexAuthMode:    config.CodexAuthSubscription,
		CodexRequireAuth: true,
		CodexHome:        t.TempDir(), // no auth.json inside
	}
	if _, _, _, err := Build(cfg); !errors.Is(err, config.ErrCodexSubscriptionAuthRequired) {
		t.Fatalf("Build() error = %v, want ErrCodexSubscriptionAuthRequired", err)
	}

	runner, _, info, pendingLogin, err := BuildWithPendingLogin(cfg)
	if err != nil {
		t.Fatalf("BuildWithPendingLogin() error = %v, want nil", err)
	}
	if !pendingLogin {
		t.Error("pendingLogin = false, want true for a never-signed-in deploy")
	}
	if runner == nil {
		t.Error("runner = nil; the bot must come up to serve /login")
	}
	if info.Name != config.AIBackendCodex {
		t.Errorf("provider = %q, want codex", info.Name)
	}
}

// TestBuildWithPendingLoginKeepsRealErrorsFatal: only the "nobody has logged in
// yet" case is recoverable from chat. A misconfiguration — an API key in
// subscription mode, billing without acknowledgement — must still refuse to boot.
func TestBuildWithPendingLoginKeepsRealErrorsFatal(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want error
	}{
		{
			name: "api key in subscription mode",
			cfg: config.Config{
				AIBackend: config.AIBackendCodex, CodexAuthMode: config.CodexAuthSubscription,
				CodexAPIKey: "sk-live", CodexRequireAuth: true,
			},
			want: config.ErrCodexSubscriptionWithAPIKey,
		},
		{
			name: "billing without acknowledgement",
			cfg: config.Config{
				AIBackend: config.AIBackendCodex, CodexAuthMode: config.CodexAuthBilling,
				CodexAPIKey: "sk-live", CodexBillingAck: false,
			},
			want: config.ErrCodexBillingAckRequired,
		},
		{
			name: "unknown auth mode",
			cfg: config.Config{
				AIBackend: config.AIBackendCodex, CodexAuthMode: "whatever",
			},
			want: config.ErrCodexUnknownAuthMode,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, pendingLogin, err := BuildWithPendingLogin(tt.cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if pendingLogin {
				t.Error("pendingLogin = true for a misconfiguration")
			}
		})
	}
}

// TestBuildWithPendingLoginAuthorizedDeploy: with a persisted login nothing is
// pending and the build is the plain one.
func TestBuildWithPendingLoginAuthorizedDeploy(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	_, _, _, pendingLogin, err := BuildWithPendingLogin(config.Config{
		AIBackend: config.AIBackendCodex, CodexAuthMode: config.CodexAuthSubscription,
		CodexRequireAuth: true, CodexHome: home,
	})
	if err != nil {
		t.Fatalf("BuildWithPendingLogin() error = %v", err)
	}
	if pendingLogin {
		t.Error("pendingLogin = true for an authorized deploy")
	}
}

// TestCodexAuthConfigMirrorsTheRunner: the login must run with the SAME child
// environment and CODEX_HOME the runner uses, or it would persist auth.json
// where later runs never look. Subscription mode also keeps CODEX_API_KEY out.
func TestCodexAuthConfigMirrorsTheRunner(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "sk-should-not-leak")
	home := t.TempDir()
	cfg := config.Config{
		AIBackend: config.AIBackendCodex, CodexAuthMode: config.CodexAuthSubscription,
		CodexBin: "/usr/local/bin/codex", CodexHome: home, CodexAccessToken: "tok",
	}
	got := CodexAuthConfig(cfg, agent.ProviderInfo{Name: config.AIBackendCodex})

	if got.Backend != config.AIBackendCodex || got.AuthMode != config.CodexAuthSubscription {
		t.Errorf("backend/auth mode = %q/%q, want codex/subscription", got.Backend, got.AuthMode)
	}
	if got.Bin != "/usr/local/bin/codex" || got.Home != home {
		t.Errorf("bin/home = %q/%q, want the configured pair", got.Bin, got.Home)
	}
	if !got.HasAccessToken {
		t.Error("HasAccessToken = false with CODEX_ACCESS_TOKEN set")
	}
	for _, kv := range got.Env {
		if strings.HasPrefix(kv, "CODEX_API_KEY=") {
			t.Fatal("CODEX_API_KEY leaked into the subscription-mode login environment")
		}
	}
}
