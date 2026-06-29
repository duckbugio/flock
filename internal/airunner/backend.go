// Package airunner builds the configured AI provider runner for adapters.
package airunner

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/codex"
	"github.com/duckbugio/flock/core/openaicompat"
	"github.com/duckbugio/flock/internal/config"
)

// ErrUnknownProvider is returned when AI_BACKEND does not name a registered
// provider or alias.
var ErrUnknownProvider = errors.New("unknown AI_BACKEND provider")

// Provider constructs one AI backend behind the common agent.Runner contract.
type Provider interface {
	agent.Provider
	Validate(config.Config) error
	Build(config.Config) (agent.Runner, agent.Options, error)
	Aliases() []string
}

// Registry maps AI_BACKEND names and aliases to providers.
type Registry struct {
	providers map[string]Provider
	aliases   map[string]string
}

// NewRegistry returns an empty provider registry.
func NewRegistry() Registry {
	return Registry{
		providers: map[string]Provider{},
		aliases:   map[string]string{},
	}
}

// DefaultRegistry returns the providers compiled into this binary.
func DefaultRegistry() Registry {
	r := NewRegistry()
	r.Register(claudeProvider{})
	r.Register(codexProvider{})
	r.Register(openAICompatProvider{})
	return r
}

// Register adds p under its canonical name and aliases.
func (r Registry) Register(p Provider) {
	name := normalizeProviderName(p.Name())
	r.providers[name] = p
	for _, alias := range p.Aliases() {
		r.aliases[normalizeProviderName(alias)] = name
	}
}

// Build returns the configured provider runner, run defaults, and metadata.
func Build(cfg config.Config) (agent.Runner, agent.Options, agent.ProviderInfo, error) {
	return DefaultRegistry().Build(cfg)
}

// Build returns the configured provider runner, run defaults, and metadata from
// this registry.
func (r Registry) Build(cfg config.Config) (agent.Runner, agent.Options, agent.ProviderInfo, error) {
	p, err := r.Provider(cfg.AIBackend)
	if err != nil {
		return nil, agent.Options{}, agent.ProviderInfo{}, err
	}
	if err := p.Validate(cfg); err != nil {
		return nil, agent.Options{}, agent.ProviderInfo{}, err
	}
	runner, opts, err := p.Build(cfg)
	if err != nil {
		return nil, agent.Options{}, agent.ProviderInfo{}, err
	}
	return runner, opts, agent.ProviderInfo{
		Name:         p.Name(),
		DisplayName:  p.DisplayName(),
		Capabilities: p.Capabilities(),
	}, nil
}

// Provider resolves a configured provider name or alias.
func (r Registry) Provider(name string) (Provider, error) {
	normalized := normalizeProviderName(name)
	if normalized == "" {
		normalized = config.AIBackendClaude
	}
	if canonical, ok := r.aliases[normalized]; ok {
		normalized = canonical
	}
	p, ok := r.providers[normalized]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, strings.TrimSpace(name))
	}
	return p, nil
}

func normalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

type claudeProvider struct{}

func (claudeProvider) Name() string        { return config.AIBackendClaude }
func (claudeProvider) DisplayName() string { return "Claude CLI" }
func (claudeProvider) Aliases() []string   { return []string{"claude-cli"} }
func (claudeProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ResumeSessions:       true,
		StreamEvents:         true,
		ToolEvents:           true,
		ShellExecution:       true,
		FileEditing:          true,
		Images:               true,
		CostReporting:        true,
		SubscriptionAuth:     true,
		APIKeyBilling:        true,
		SupportsApprovalMode: true,
	}
}
func (claudeProvider) Validate(config.Config) error { return nil }
func (claudeProvider) Build(cfg config.Config) (agent.Runner, agent.Options, error) {
	return claude.New(cfg.ClaudeBin), agent.Options{
		Model:    cfg.ClaudeModel,
		MaxTurns: cfg.ClaudeMaxTurns,
		Effort:   cfg.ClaudeEffort,
		Env:      ClaudeEnv(cfg),
	}, nil
}

type codexProvider struct{}

func (codexProvider) Name() string        { return config.AIBackendCodex }
func (codexProvider) DisplayName() string { return "Codex CLI" }
func (codexProvider) Aliases() []string   { return []string{"codex-cli"} }
func (codexProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ResumeSessions:       true,
		StreamEvents:         true,
		ToolEvents:           true,
		ShellExecution:       true,
		FileEditing:          true,
		SubscriptionAuth:     true,
		APIKeyBilling:        true,
		RequiresGitRepo:      true,
		SupportsSandbox:      true,
		SupportsApprovalMode: true,
	}
}
func (codexProvider) Validate(cfg config.Config) error {
	cfg.AIBackend = config.AIBackendCodex
	return cfg.ValidateCodex()
}
func (codexProvider) Build(cfg config.Config) (agent.Runner, agent.Options, error) {
	return codex.New(codex.Config{
			Bin:              cfg.CodexBin,
			Sandbox:          cfg.CodexSandbox,
			ApprovalPolicy:   cfg.CodexApprovalPolicy,
			AuthMode:         cfg.CodexAuthModeName(),
			SkipGitRepoCheck: cfg.CodexSkipGitRepoCheck,
			ExtraArgs:        strings.Fields(cfg.CodexExtraArgs),
		}),
		agent.Options{
			Model: cfg.CodexModel,
			Env:   CodexEnv(cfg),
		},
		nil
}

type openAICompatProvider struct{}

func (openAICompatProvider) Name() string        { return config.AIBackendOpenAICompat }
func (openAICompatProvider) DisplayName() string { return "OpenAI-compatible API" }
func (openAICompatProvider) Aliases() []string   { return []string{"openai", "openai-compatible-api"} }
func (openAICompatProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		StreamEvents:  true,
		APIKeyBilling: true,
	}
}
func (openAICompatProvider) Validate(cfg config.Config) error {
	cfg.AIBackend = config.AIBackendOpenAICompat
	return cfg.ValidateOpenAICompat()
}
func (openAICompatProvider) Build(cfg config.Config) (agent.Runner, agent.Options, error) {
	return openaicompat.New(openaicompat.Config{
			BaseURL: cfg.OpenAICompatBaseURL,
			Model:   cfg.OpenAICompatModel,
			APIKey:  cfg.OpenAICompatResolvedAPIKey(),
			Timeout: cfg.OpenAICompatTimeout(),
		}),
		agent.Options{
			Model: cfg.OpenAICompatModel,
		},
		nil
}

// ClaudeEnv derives the child process environment for the Claude CLI.
func ClaudeEnv(cfg config.Config) []string {
	env := os.Environ()
	if strings.TrimSpace(cfg.ClaudeCodeOAuthToken) != "" {
		env = setEnv(env, "CLAUDE_CODE_OAUTH_TOKEN", cfg.ClaudeCodeOAuthToken)
	}
	if strings.TrimSpace(cfg.AnthropicAPIKey) != "" {
		env = setEnv(env, "ANTHROPIC_API_KEY", cfg.AnthropicAPIKey)
	}
	return env
}

// CodexEnv derives the child process environment for the Codex CLI. In
// subscription mode it strips CODEX_API_KEY even if the outer process has one.
func CodexEnv(cfg config.Config) []string {
	env := os.Environ()
	if strings.TrimSpace(cfg.CodexHome) != "" {
		env = setEnv(env, "CODEX_HOME", cfg.CodexHome)
	}
	switch cfg.CodexAuthModeName() {
	case config.CodexAuthBilling:
		if strings.TrimSpace(cfg.CodexAPIKey) != "" {
			env = setEnv(env, "CODEX_API_KEY", cfg.CodexAPIKey)
		}
	case config.CodexAuthSubscription:
		env = removeEnv(env, "CODEX_API_KEY")
		if strings.TrimSpace(cfg.CodexAccessToken) != "" {
			env = setEnv(env, "CODEX_ACCESS_TOKEN", cfg.CodexAccessToken)
		}
	}
	return env
}

func setEnv(env []string, key, value string) []string {
	env = removeEnv(env, key)
	return append(env, key+"="+value)
}

func removeEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
