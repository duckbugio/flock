// Package config loads the flock adapter configuration from environment
// variables, mirroring the names used by the existing Python adapter so deploy
// tooling stays compatible.
package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	env "github.com/caarlos0/env/v11"
)

// Config holds all runtime configuration sourced from the environment. Field
// names and env keys mirror adapters/telegram/.env.example for deploy parity.
type Config struct {
	// Required.
	TelegramBotToken    string `env:"TELEGRAM_BOT_TOKEN,required"`
	TelegramBotUsername string `env:"TELEGRAM_BOT_USERNAME"`

	// Telegram user IDs allowed to use the bot (comma-separated).
	AllowedUsers []int64 `env:"ALLOWED_USERS" envSeparator:","`

	// Claude auth — not hard-required at Stage 0.
	ClaudeCodeOAuthToken string `env:"CLAUDE_CODE_OAUTH_TOKEN"`
	AnthropicAPIKey      string `env:"ANTHROPIC_API_KEY"`
	ClaudeModel          string `env:"CLAUDE_MODEL" envDefault:"claude-opus-4-8"`
	ClaudeBin            string `env:"CLAUDE_BIN" envDefault:"claude"`
	ClaudeMaxTurns       int    `env:"CLAUDE_MAX_TURNS" envDefault:"40"`

	// ClaudeTimeoutSeconds bounds a single run's delivery: it is applied as a
	// per-run context deadline. When it elapses the run is cancelled (delivery
	// stops) but the chat's stored session_id is PRESERVED, so the next message
	// resumes seamlessly via --resume (plan §7.3). 0 disables the deadline.
	ClaudeTimeoutSeconds int `env:"CLAUDE_TIMEOUT_SECONDS" envDefault:"0"`

	// Workspace + behavior.
	ApprovedDirectory     string `env:"APPROVED_DIRECTORY" envDefault:"/workspace"`
	LogLevel              string `env:"LOG_LEVEL" envDefault:"INFO"`
	MaxConcurrentChatRuns int    `env:"MAX_CONCURRENT_CHAT_RUNS" envDefault:"4"`

	// SessionStorePath is the JSON file persisting chatID -> session_id for
	// --resume continuity across messages and restarts (plan §4). When empty it
	// defaults to <APPROVED_DIRECTORY>/sessions.json (see SessionStoreFile), which
	// lives under the workspace data dir, never inside a repo.
	SessionStorePath string `env:"SESSION_STORE_PATH"`

	// Guardrails (Stage 9b). A per-user fixed-window RATE LIMIT and a per-user
	// cumulative COST CAP enforced on the inbound message path. The defaults
	// mirror adapters/telegram/.env.example.
	//
	// RateLimitRequests is the max requests a single user may make within
	// RateLimitWindowSeconds; either being non-positive disables the rate limit
	// (see RateLimitEnabled). CLAUDE_MAX_COST_PER_USER is the cumulative USD cap
	// that, once crossed, denies a user's NEXT request (reactive — the crossing
	// request still ran).
	RateLimitRequests    int     `env:"RATE_LIMIT_REQUESTS" envDefault:"30"`
	RateLimitWindowSecs  int     `env:"RATE_LIMIT_WINDOW" envDefault:"60"`
	ClaudeMaxCostPerUser float64 `env:"CLAUDE_MAX_COST_PER_USER" envDefault:"1000.0"`

	// CostStorePath is the JSON file persisting userID -> accumulated USD for the
	// cumulative cost cap. When empty it defaults to <APPROVED_DIRECTORY>/costs.json
	// (see CostStoreFile), alongside the session store under the workspace data dir.
	CostStorePath string `env:"COST_STORE_PATH"`

	// RequireGroupMention gates group-chat handling: when false (the default,
	// matching the Python adapter's .env.example / mention_gate.py) the bot
	// answers every group message; when true it only acts on a message that
	// @mentions the bot or replies to one of the bot's own messages. DMs are
	// always handled regardless. See plan §5.
	RequireGroupMention bool `env:"REQUIRE_GROUP_MENTION" envDefault:"false"`

	// Team-loop knobs rendered into each workspace's CLAUDE.md (mirrors the
	// values the Python entrypoint substituted). Kept as strings since they are
	// substituted verbatim into the template.
	PrePRCycles    string `env:"PRE_PR_CYCLES" envDefault:"5"`
	PrReviewCycles string `env:"PR_REVIEW_CYCLES" envDefault:"10"`
	EnablePRReview string `env:"ENABLE_PR_REVIEW" envDefault:"false"`

	// Source paths for the shared team config baked into the image (see the
	// Dockerfile's /opt/duck layout). Rendered per chat by core/workspace.
	TeamTemplatePath string `env:"TEAM_TEMPLATE_PATH" envDefault:"/opt/duck/CLAUDE.workspace.md.tmpl"`
	TeamAgentsDir    string `env:"TEAM_AGENTS_DIR" envDefault:"/opt/duck/agents"`

	// Git startup wiring (core/gitsetup). Mirrors the Python entrypoint's git
	// portion. GitToken/GitUser also feed the inline credential helper and the
	// Gitea poller; they are read from the env, never written to a config file.
	GitHost        string `env:"GIT_HOST"`
	GitScheme      string `env:"GIT_SCHEME" envDefault:"https"`
	GitUser        string `env:"GIT_USER"`
	GitToken       string `env:"GIT_TOKEN"`
	GitAuthorName  string `env:"GIT_AUTHOR_NAME" envDefault:"AI Team"`
	GitAuthorEmail string `env:"GIT_AUTHOR_EMAIL" envDefault:"ai@example.com"`

	// Gitea notification poller (core/poller). Only active when PR review is
	// enabled and the API URL + token are set (see PollerEnabled).
	GiteaAPIURL       string `env:"GITEA_API_URL"`
	GiteaPollInterval int    `env:"GITEA_POLL_INTERVAL" envDefault:"90"`

	// Voice transcription (core/voice). Disabled by default; when enabled,
	// VoiceProvider selects the transcription backend and the matching key/command
	// must be set. Mirrors the VOICE_* keys in adapters/telegram/.env.example.
	EnableVoiceMessages bool   `env:"ENABLE_VOICE_MESSAGES" envDefault:"false"`
	VoiceProvider       string `env:"VOICE_PROVIDER" envDefault:"mistral"`
	MistralAPIKey       string `env:"MISTRAL_API_KEY"`
	OpenAIAPIKey        string `env:"OPENAI_API_KEY"`
	VoiceMistralModel   string `env:"VOICE_MISTRAL_MODEL" envDefault:"voxtral-mini-latest"`
	VoiceOpenAIModel    string `env:"VOICE_OPENAI_MODEL" envDefault:"whisper-1"`
	VoiceLocalCommand   string `env:"VOICE_LOCAL_COMMAND"`

	// MaxUploadBytes caps an inbound document/photo download (the Telegram Bot API
	// getFile download limit is 20 MB). An oversize file is rejected with a single
	// friendly notice rather than downloaded. A non-positive value falls back to the
	// uploader's built-in default (see internal/telegram.defaultMaxUploadBytes).
	MaxUploadBytes int64 `env:"MAX_UPLOAD_BYTES" envDefault:"20971520"`

	// MaxOutboxBytes caps a single outbound file the outbox sweeper delivers after
	// a run (a generated report/export/artifact). An oversize file is skipped with
	// a logged warning rather than sent. A non-positive value falls back to the
	// sweeper's built-in default (see internal/telegram.defaultMaxOutboxBytes).
	MaxOutboxBytes int64 `env:"MAX_OUTBOX_BYTES" envDefault:"20971520"`

	// MaxOutboxFiles bounds how many files a single outbox sweep delivers, so a run
	// that fills the outbox cannot spam the chat. Files beyond the cap are left in
	// place for a future sweep. A non-positive value falls back to the sweeper's
	// built-in default (see internal/telegram.defaultMaxOutboxFiles).
	MaxOutboxFiles int `env:"MAX_OUTBOX_FILES" envDefault:"10"`
}

// Load parses the configuration from the process environment.
func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// SlogLevel maps the configured LOG_LEVEL word (case-insensitive) to a
// slog.Level, defaulting to slog.LevelInfo for unknown values.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToUpper(strings.TrimSpace(c.LogLevel)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ClaudeTimeout returns the per-run delivery deadline as a Duration, or 0 when
// no timeout is configured (a run may then take as long as the CLI needs).
func (c Config) ClaudeTimeout() time.Duration {
	if c.ClaudeTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.ClaudeTimeoutSeconds) * time.Second
}

// SessionStoreFile returns the path to the JSON session store, defaulting to
// <ApprovedDirectory>/sessions.json when SESSION_STORE_PATH is unset so it lives
// under the workspace data dir rather than inside any repo.
func (c Config) SessionStoreFile() string {
	if strings.TrimSpace(c.SessionStorePath) != "" {
		return c.SessionStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "sessions.json")
}

// RateLimitWindow returns the per-user rate-limit window as a Duration, or 0
// when the configured window is not positive (which disables the rate limit).
func (c Config) RateLimitWindow() time.Duration {
	if c.RateLimitWindowSecs <= 0 {
		return 0
	}
	return time.Duration(c.RateLimitWindowSecs) * time.Second
}

// RateLimitEnabled reports whether the per-user rate limit is active: it needs a
// positive request budget AND a positive window. Either being non-positive turns
// the limiter off (it then always allows).
func (c Config) RateLimitEnabled() bool {
	return c.RateLimitRequests > 0 && c.RateLimitWindowSecs > 0
}

// CostCapEnabled reports whether the cumulative per-user cost cap is active: a
// positive CLAUDE_MAX_COST_PER_USER. A non-positive cap disables the gate (every
// request is allowed regardless of accumulated spend).
func (c Config) CostCapEnabled() bool {
	return c.ClaudeMaxCostPerUser > 0
}

// CostStoreFile returns the path to the JSON per-user cost store, defaulting to
// <ApprovedDirectory>/costs.json when COST_STORE_PATH is unset so it lives under
// the workspace data dir alongside the session store, never inside any repo.
func (c Config) CostStoreFile() string {
	if strings.TrimSpace(c.CostStorePath) != "" {
		return c.CostStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "costs.json")
}

// PRReviewEnabled reports whether the published-PR review/fix loop is enabled,
// parsing the ENABLE_PR_REVIEW string the same way the Python adapter does:
// true when the lowercased value is one of "1", "true", "yes", "on".
func (c Config) PRReviewEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(c.EnablePRReview)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// GiteaPollDuration returns the poll period as a Duration, floored to 30s (like
// the Python max(30, ...)), or 0 when the configured interval is not positive.
func (c Config) GiteaPollDuration() time.Duration {
	if c.GiteaPollInterval <= 0 {
		return 0
	}
	d := time.Duration(c.GiteaPollInterval) * time.Second
	if d < 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// PollerEnabled reports whether the Gitea notification poller should run: PR
// review enabled AND the API URL + token configured AND a positive interval.
func (c Config) PollerEnabled() bool {
	return c.PRReviewEnabled() && c.GiteaAPIURL != "" && c.GitToken != "" && c.GiteaPollInterval > 0
}

// IsAllowed reports whether the given Telegram user ID is in the allow-list.
func (c Config) IsAllowed(userID int64) bool {
	for _, id := range c.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}
