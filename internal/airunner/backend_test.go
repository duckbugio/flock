//nolint:testpackage // whitebox tests exercise registry construction directly.
package airunner

import (
	"errors"
	"testing"

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
