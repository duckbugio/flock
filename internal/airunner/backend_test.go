//nolint:testpackage // whitebox tests exercise registry construction directly.
package airunner

import (
	"errors"
	"reflect"
	"strings"
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

// TestTurnLimitEnv checks the turn-cap knob is named only for the provider that
// actually applies one, so a message can never point at a variable that does
// nothing for the configured backend.
func TestTurnLimitEnv(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{config.AIBackendClaude, config.ClaudeMaxTurnsEnv},
		{"claude-cli", config.ClaudeMaxTurnsEnv},
		{"  CLAUDE  ", config.ClaudeMaxTurnsEnv},
		{"", config.ClaudeMaxTurnsEnv}, // unset AI_BACKEND defaults to claude
		{config.AIBackendCodex, ""},
		{"codex-cli", ""},
		{config.AIBackendOpenAICompat, ""},
		{"openai", ""},
		{"qwen", ""}, // unknown provider names no knob
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := TurnLimitEnv(tt.provider); got != tt.want {
				t.Fatalf("TurnLimitEnv(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

// turnCapFieldSuffix is the naming convention every turn-cap config field
// follows (ClaudeMaxTurns today), so turnCapProbeConfig can fill the ones a
// provider added later brings with it.
const turnCapFieldSuffix = "MaxTurns"

// testTurnCap is the positive cap fed to every turn-cap config field.
const testTurnCap = 40

// TestTurnLimitEnvMatchesProviderTurnCap guards the fact TurnLimitEnv hardcodes:
// that Claude is the only backend actually passing a turn cap down. That fact
// lives in the providers' Build methods, while TurnLimitEnv and TestTurnLimitEnv
// both just repeat it — so nothing today notices the two drifting apart. Concretely:
// a provider that starts setting agent.Options.MaxTurns really does enforce a cap,
// yet the terminal message would stay silent about the knob that raises it, and
// every other test would still pass. Asserting the biconditional over the whole
// registry also covers providers that do not exist yet: enforcing a cap and naming
// its env var must always come together, in both directions — provided the new
// provider's cap field follows turnCapFieldSuffix. A cap wired from a differently
// named field reads back as zero from the probe config and is NOT covered.
func TestTurnLimitEnvMatchesProviderTurnCap(t *testing.T) {
	registry := DefaultRegistry()
	if len(registry.providers) == 0 {
		t.Fatal("DefaultRegistry() registered no providers")
	}
	for name, p := range registry.providers {
		t.Run(name, func(t *testing.T) {
			cfg := turnCapProbeConfig(t)
			cfg.AIBackend = name
			_, opts, _, err := registry.Build(cfg)
			if err != nil {
				// A provider that fails to build proves nothing: opts.MaxTurns stays zero
				// and the assertion below would pass vacuously. Every provider registered
				// today builds from turnCapProbeConfig; one that needs more (real
				// credentials, say) must either get them there or be skipped right here
				// with the reason spelled out — never let the error slide.
				t.Fatalf("Build(%q) error = %v, want a provider that builds from turnCapProbeConfig", name, err)
			}
			capped := opts.MaxTurns > 0
			knob := TurnLimitEnv(p.Name())
			if capped != (knob != "") {
				t.Fatalf("provider %q: Options.MaxTurns = %d, TurnLimitEnv = %q — a provider that caps turns must "+
					"name the env var that sets it, and only such a provider may name one",
					p.Name(), opts.MaxTurns, knob)
			}
		})
	}
}

// turnCapProbeConfig returns a config every registered provider can build from,
// with EVERY turn-cap field set to a positive value. Those fields are filled by
// reflection rather than named one by one on purpose: a provider that gains a cap
// brings its own <Provider>MaxTurns field, and a probe config that only sets
// ClaudeMaxTurns would read back a zero cap for it and pass vacuously — missing
// exactly the drift the caller exists to catch. The credentials are the same
// throwaway values the other Build tests use.
func turnCapProbeConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		ClaudeBin:              "claude",
		CodexBin:               "codex",
		CodexAuthMode:          config.CodexAuthSubscription,
		CodexRequireAuth:       false,
		OpenAICompatBaseURL:    "https://example.test/v1",
		OpenAICompatModel:      "qwen-plus",
		OpenAICompatAuthMode:   config.OpenAICompatAuthAPIKey,
		OpenAICompatAPIKey:     "sk-test",
		OpenAICompatBillingAck: true,
	}
	value := reflect.ValueOf(&cfg).Elem()
	fields := value.Type()
	for i := 0; i < fields.NumField(); i++ {
		field := fields.Field(i)
		if !field.IsExported() || !strings.HasSuffix(field.Name, turnCapFieldSuffix) ||
			field.Type.Kind() != reflect.Int {
			continue
		}
		value.Field(i).SetInt(testTurnCap)
	}
	// Counting the fields filled above cannot catch a rename: GoalEvalMaxTurns — the
	// goal evaluator's own cap — matches the suffix while feeding no provider, so the
	// count never reaches zero. Assert the one field that DOES feed a provider today,
	// so renaming it away from the convention fails here, naming the cause, instead of
	// surfacing as a Claude backend that mysteriously stopped capping turns.
	if cfg.ClaudeMaxTurns != testTurnCap {
		t.Fatalf("ClaudeMaxTurns = %d, want %d: no field named <Provider>%s fed the Claude backend, "+
			"so the turn-cap naming convention changed and this probe no longer sets a cap",
			cfg.ClaudeMaxTurns, testTurnCap, turnCapFieldSuffix)
	}
	return cfg
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
