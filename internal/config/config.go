// Package config loads the flock adapter configuration from environment
// variables, mirroring the names used by the existing Python adapter so deploy
// tooling stays compatible.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	env "github.com/caarlos0/env/v11"
)

// ErrMissingTelegramToken is returned by ValidateTelegram when TELEGRAM_BOT_TOKEN
// is unset, mirroring the previous env "required" behavior but deferred so a
// non-Telegram binary (e.g. cmd/duck-vk) can share the same Config without
// demanding the Telegram token.
var ErrMissingTelegramToken = errors.New("TELEGRAM_BOT_TOKEN is required")

// ErrMissingVKToken is returned by ValidateVK when VK_BOT_TOKEN is unset.
var ErrMissingVKToken = errors.New("VK_BOT_TOKEN is required")

// ErrMissingVKGroupID is returned by ValidateVK when VK_GROUP_ID is unset/zero;
// the community id is needed for groups.getLongPollServer and the mention parse.
var ErrMissingVKGroupID = errors.New("VK_GROUP_ID is required")

// Config holds all runtime configuration sourced from the environment. Field
// names and env keys mirror adapters/telegram/.env.example for deploy parity.
// The per-transport tokens are parsed but NOT marked env-required: each binary
// validates only its own transport (ValidateTelegram / ValidateVK) so a VK
// deployment need not set TELEGRAM_BOT_TOKEN and vice versa, while the shared
// core config is parsed once and reused.
type Config struct {
	// Telegram (cmd/flock-telegram). Validated by ValidateTelegram, not env-required.
	TelegramBotToken    string `env:"TELEGRAM_BOT_TOKEN"`
	TelegramBotUsername string `env:"TELEGRAM_BOT_USERNAME"`

	// Telegram user IDs allowed to use the bot (comma-separated).
	AllowedUsers []int64 `env:"ALLOWED_USERS" envSeparator:","`

	// VK (cmd/duck-vk). VKBotToken is the community access token; VKGroupID is the
	// community id (used for groups.getLongPollServer and the [club<id>|...] mention
	// parse); VKAllowedUsers is the comma-separated allow-list of VK user ids. These
	// are validated by ValidateVK, not env-required, so the Telegram binary need not
	// set them. REQUIRE_GROUP_MENTION is shared (reused, not duplicated).
	VKBotToken     string  `env:"VK_BOT_TOKEN"`
	VKGroupID      int64   `env:"VK_GROUP_ID"`
	VKAllowedUsers []int64 `env:"VK_ALLOWED_USERS" envSeparator:","`

	// Claude auth — not hard-required at Stage 0.
	ClaudeCodeOAuthToken string `env:"CLAUDE_CODE_OAUTH_TOKEN"`
	AnthropicAPIKey      string `env:"ANTHROPIC_API_KEY"`
	ClaudeModel          string `env:"CLAUDE_MODEL" envDefault:"claude-opus-4-8"`
	ClaudeBin            string `env:"CLAUDE_BIN" envDefault:"claude"`
	ClaudeMaxTurns       int    `env:"CLAUDE_MAX_TURNS" envDefault:"40"`

	// ClaudeTimeoutSeconds bounds a single run's delivery: it is applied as a
	// per-run context deadline. When it elapses the run is cancelled (delivery
	// stops) but the chat's stored session_id is PRESERVED, so the next message
	// resumes seamlessly via --resume (plan §7.3). The default bounds a run to
	// 9 hours (32400s); set CLAUDE_TIMEOUT_SECONDS=0 to disable the deadline, or
	// any other value to override.
	ClaudeTimeoutSeconds int `env:"CLAUDE_TIMEOUT_SECONDS" envDefault:"32400"`

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

	// EnableRichMessages toggles Bot API 10.1 rich rendering in the Telegram
	// adapter (structured blocks + native rich draft). Default false: the adapter
	// uses the legacy MarkdownToHTML/plain path and behaves exactly as before. When
	// true it is surfaced via the Telegram Capabilities().CanSendRich flag; the rich
	// path still falls back to MarkdownToHTML/plain on any error, so flipping this on
	// can never break delivery. VK ignores it (no rich support). See
	// docs/rich-messages-plan.md.
	EnableRichMessages bool `env:"ENABLE_RICH_MESSAGES" envDefault:"false"`

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

	// StarNudgeRepo is the "owner/repo" the post-task star nudge targets. The
	// nudge invites the user to star the project on GitHub and is GitHub-only: it
	// is active only when GIT_HOST is github.com and a GIT_TOKEN is set (see
	// StarNudgeEnabled). The token must have star-write scope for the button to
	// work (classic public_repo, or a fine-grained token with the "Starring"
	// account permission).
	StarNudgeRepo string `env:"STAR_NUDGE_REPO" envDefault:"duckbugio/flock"`

	// StarNudgeStorePath is the JSON file persisting the global "starred resolved"
	// flag so the nudge stops once the repo is known starred. When empty it
	// defaults to <APPROVED_DIRECTORY>/star_nudge.json (see StarNudgeStoreFile),
	// alongside the session/cost stores under the workspace data dir, never inside
	// a repo.
	StarNudgeStorePath string `env:"STAR_NUDGE_STORE_PATH"`

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

// minGiteaPollInterval floors the Gitea poll period, mirroring the Python
// max(30, ...).
const minGiteaPollInterval = 30 * time.Second

// GiteaPollDuration returns the poll period as a Duration, floored to 30s (like
// the Python max(30, ...)), or 0 when the configured interval is not positive.
func (c Config) GiteaPollDuration() time.Duration {
	if c.GiteaPollInterval <= 0 {
		return 0
	}
	d := time.Duration(c.GiteaPollInterval) * time.Second
	if d < minGiteaPollInterval {
		return minGiteaPollInterval
	}
	return d
}

// PollerEnabled reports whether the Gitea notification poller should run: PR
// review enabled AND the API URL + token configured AND a positive interval.
func (c Config) PollerEnabled() bool {
	return c.PRReviewEnabled() && c.GiteaAPIURL != "" && c.GitToken != "" && c.GiteaPollInterval > 0
}

// gitHubHost is the GIT_HOST value (case-insensitive) that enables the
// GitHub-only star nudge.
const gitHubHost = "github.com"

// starRepoParts is the number of "/"-separated segments a STAR_NUDGE_REPO must
// have: exactly "owner/repo".
const starRepoParts = 2

// StarNudgeStoreFile returns the path to the JSON star-nudge flag store,
// defaulting to <ApprovedDirectory>/star_nudge.json when STAR_NUDGE_STORE_PATH is
// unset so it lives under the workspace data dir alongside the session/cost
// stores, never inside any repo.
func (c Config) StarNudgeStoreFile() string {
	if strings.TrimSpace(c.StarNudgeStorePath) != "" {
		return c.StarNudgeStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "star_nudge.json")
}

// StarNudgeRepoParts splits StarNudgeRepo into owner and repo, returning ok=false
// when it is not a well-formed "owner/repo" (e.g. empty, missing slash, or a
// blank segment). Surrounding whitespace is trimmed.
func (c Config) StarNudgeRepoParts() (owner, repo string, ok bool) {
	parts := strings.Split(strings.TrimSpace(c.StarNudgeRepo), "/")
	if len(parts) != starRepoParts {
		return "", "", false
	}
	owner, repo = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// StarNudgeEnabled reports whether the post-task star nudge should run: the
// deployment is wired to github.com, a GIT_TOKEN is set, and STAR_NUDGE_REPO
// parses as "owner/repo". Any other host (Gitea/GitLab) or a missing token turns
// the feature off — the gate IS the off switch, so no separate flag is needed.
func (c Config) StarNudgeEnabled() bool {
	if !strings.EqualFold(strings.TrimSpace(c.GitHost), gitHubHost) {
		return false
	}
	if c.GitToken == "" {
		return false
	}
	_, _, ok := c.StarNudgeRepoParts()
	return ok
}

// IsAllowed reports whether the given Telegram user ID is in the allow-list.
func (c Config) IsAllowed(userID int64) bool {
	return containsID(c.AllowedUsers, userID)
}

// IsVKAllowed reports whether the given VK user ID is in the VK allow-list.
// VK_ALLOWED_USERS is a separate list from the Telegram ALLOWED_USERS so the two
// transports' deploys stay independent (a VK user id is unrelated to a Telegram
// one).
func (c Config) IsVKAllowed(userID int64) bool {
	return containsID(c.VKAllowedUsers, userID)
}

// containsID reports whether ids contains userID.
func containsID(ids []int64, userID int64) bool {
	for _, id := range ids {
		if id == userID {
			return true
		}
	}
	return false
}

// ValidateTelegram checks the fields the Telegram binary requires at startup. It
// is called by cmd/flock-telegram after Load, replacing the previous env
// "required" tag on TELEGRAM_BOT_TOKEN so the shared Config can be parsed by a
// non-Telegram binary too.
func (c Config) ValidateTelegram() error {
	if strings.TrimSpace(c.TelegramBotToken) == "" {
		return ErrMissingTelegramToken
	}
	return nil
}

// ValidateVK checks the fields the VK binary requires at startup (the community
// access token and the community id). It is called by cmd/duck-vk after Load.
func (c Config) ValidateVK() error {
	if strings.TrimSpace(c.VKBotToken) == "" {
		return ErrMissingVKToken
	}
	if c.VKGroupID == 0 {
		return ErrMissingVKGroupID
	}
	return nil
}
