// Package config loads the flock adapter configuration from environment
// variables, mirroring the names used by the existing Python adapter so deploy
// tooling stays compatible.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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

// AI backend names supported by the shared chat runner wiring.
const (
	AIBackendClaude       = "claude"
	AIBackendCodex        = "codex"
	AIBackendOpenAICompat = "openai-compatible"
)

// Codex auth modes. "subscription" uses a persisted Codex login/CODEX_HOME or
// access token; "billing" uses CODEX_API_KEY and must be opted into explicitly.
const (
	CodexAuthSubscription = "subscription"
	CodexAuthBilling      = "billing"
)

const (
	// OpenAICompatAuthAPIKey authenticates OpenAI-compatible providers with a
	// bearer API key.
	OpenAICompatAuthAPIKey = "api_key"
)

var (
	// ErrCodexSubscriptionWithAPIKey is returned when subscription auth is mixed
	// with usage-based API billing credentials.
	ErrCodexSubscriptionWithAPIKey = errors.New("CODEX_API_KEY is not allowed when CODEX_AUTH_MODE=subscription")
	// ErrCodexSubscriptionAuthRequired is returned when Codex subscription mode
	// has neither an access token nor persisted CLI login state.
	ErrCodexSubscriptionAuthRequired = errors.New(
		"codex subscription auth requires CODEX_ACCESS_TOKEN or CODEX_HOME/auth.json",
	)
	// ErrCodexBillingAckRequired is returned until API billing mode is explicitly
	// acknowledged.
	ErrCodexBillingAckRequired = errors.New("CODEX_BILLING_ACK=true is required when CODEX_AUTH_MODE=billing")
	// ErrCodexBillingAPIKeyRequired is returned when billing mode has no API key.
	ErrCodexBillingAPIKeyRequired = errors.New("CODEX_API_KEY is required when CODEX_AUTH_MODE=billing")
	// ErrCodexUnknownAuthMode is returned for unsupported Codex auth modes.
	ErrCodexUnknownAuthMode = errors.New("unknown CODEX_AUTH_MODE")

	// ErrOpenAICompatBaseURLRequired is returned when the OpenAI-compatible
	// provider is enabled without a base URL.
	ErrOpenAICompatBaseURLRequired = errors.New("OPENAI_COMPAT_BASE_URL is required when AI_BACKEND=openai-compatible")
	// ErrOpenAICompatModelRequired is returned when the OpenAI-compatible
	// provider is enabled without a model.
	ErrOpenAICompatModelRequired = errors.New("OPENAI_COMPAT_MODEL is required when AI_BACKEND=openai-compatible")
	// ErrOpenAICompatAPIKeyRequired is returned when API-key auth has no key in
	// config or in the named environment variable.
	ErrOpenAICompatAPIKeyRequired = errors.New("openai-compatible API key is required when OPENAI_COMPAT_AUTH_MODE=api_key")
	// ErrOpenAICompatBillingAckRequired is returned until OpenAI-compatible API
	// billing is explicitly acknowledged.
	ErrOpenAICompatBillingAckRequired = errors.New("OPENAI_COMPAT_BILLING_ACK=true is required when AI_BACKEND=openai-compatible")
	// ErrOpenAICompatUnknownAuthMode is returned for unsupported auth modes.
	ErrOpenAICompatUnknownAuthMode = errors.New("unknown OPENAI_COMPAT_AUTH_MODE")
)

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
	// ClaudeEffort is the reasoning effort level passed to the claude CLI (set by
	// roost via CLAUDE_EFFORT). Standard levels (low, medium, high, xhigh, max) map
	// to --effort; the special value "ultracode" enables Claude Code's ultracode
	// setting (via --settings). Empty (the default) passes nothing, so the model's
	// default effort applies. The mapping/validation lives in core/claude buildArgs.
	ClaudeEffort string `env:"CLAUDE_EFFORT"`

	// AIBackend selects the command runner. "claude" preserves the existing
	// Claude CLI path; "codex" runs the Codex CLI through the same chat.Service
	// event contract.
	AIBackend string `env:"AI_BACKEND" envDefault:"claude"`

	// Codex CLI integration. Subscription mode deliberately rejects CODEX_API_KEY
	// so a ChatGPT/Codex subscription deploy cannot silently switch to API billing.
	CodexBin              string `env:"CODEX_BIN" envDefault:"codex"`
	CodexModel            string `env:"CODEX_MODEL" envDefault:"gpt-5.5"`
	CodexHome             string `env:"CODEX_HOME" envDefault:"/home/claude/.codex"`
	CodexAuthMode         string `env:"CODEX_AUTH_MODE" envDefault:"subscription"`
	CodexAccessToken      string `env:"CODEX_ACCESS_TOKEN"`
	CodexAPIKey           string `env:"CODEX_API_KEY"`
	CodexRequireAuth      bool   `env:"CODEX_REQUIRE_AUTH" envDefault:"true"`
	CodexBillingAck       bool   `env:"CODEX_BILLING_ACK" envDefault:"false"`
	CodexSandbox          string `env:"CODEX_SANDBOX" envDefault:"workspace-write"`
	CodexApprovalPolicy   string `env:"CODEX_APPROVAL_POLICY" envDefault:"never"`
	CodexSkipGitRepoCheck bool   `env:"CODEX_SKIP_GIT_REPO_CHECK" envDefault:"false"`
	CodexExtraArgs        string `env:"CODEX_EXTRA_ARGS"`

	// OpenAI-compatible API integration. This is an answer-only provider in v1:
	// it can stream/finalize text through the common agent contract, but it does
	// not edit files or run shell tools unless a future controlled tool loop is
	// added.
	OpenAICompatBaseURL        string `env:"OPENAI_COMPAT_BASE_URL"`
	OpenAICompatModel          string `env:"OPENAI_COMPAT_MODEL"`
	OpenAICompatAPIKeyEnv      string `env:"OPENAI_COMPAT_API_KEY_ENV" envDefault:"OPENAI_COMPAT_API_KEY"`
	OpenAICompatAPIKey         string `env:"OPENAI_COMPAT_API_KEY"`
	OpenAICompatAuthMode       string `env:"OPENAI_COMPAT_AUTH_MODE" envDefault:"api_key"`
	OpenAICompatBillingAck     bool   `env:"OPENAI_COMPAT_BILLING_ACK" envDefault:"false"`
	OpenAICompatTimeoutSeconds int    `env:"OPENAI_COMPAT_TIMEOUT_SECONDS" envDefault:"300"`

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

	// ShutdownDrainSeconds bounds the graceful-drain window on SIGTERM: how long
	// the dispatcher WAITS for in-flight runs to finish on their own before
	// cancelling the survivors (a run that finishes within the window delivers its
	// real answer, not "⏹ Stopped."). It must stay strictly below the Docker
	// stop_grace_period (75s) so the process self-exits before SIGKILL. The default
	// is 45s; set SHUTDOWN_DRAIN_SECONDS to override.
	ShutdownDrainSeconds int `env:"SHUTDOWN_DRAIN_SECONDS" envDefault:"45"`

	// PendingStorePath is the JSON file persisting chatID -> interrupted-run marker
	// (prompt + anchor message id + start time) so a run killed mid-flight on a
	// deploy is auto-resumed on the next startup. When empty it defaults to
	// <APPROVED_DIRECTORY>/pending.json (see PendingStoreFile), which lives under
	// the workspace data dir — a persisted named volume, so the marker survives a
	// --force-recreate — never inside a repo.
	PendingStorePath string `env:"PENDING_STORE_PATH"`

	// SessionStorePath is the JSON file persisting chatID -> session_id for
	// --resume continuity across messages and restarts (plan §4). When empty it
	// defaults to <APPROVED_DIRECTORY>/sessions.json (see SessionStoreFile), which
	// lives under the workspace data dir, never inside a repo.
	SessionStorePath string `env:"SESSION_STORE_PATH"`

	// EnableScheduler gates the /schedule command and its background cron
	// scheduler. ON by default — durable per-chat cron jobs work out of the box.
	// Set ENABLE_SCHEDULER=false to disable: no schedule store is opened and no
	// scheduler goroutine runs (a true no-op), and /schedule (still a reserved,
	// bot-owned command) replies with a "disabled" notice rather than reaching the
	// model. The off switch is kept on purpose: scheduled jobs run UNATTENDED and
	// incur model cost, so an operator can kill the whole scheduler with one env
	// change. Job creation is still always a deliberate /schedule add (allow-list
	// gated, capped, cost-metered), so default-on never schedules anything by
	// itself.
	EnableScheduler bool `env:"ENABLE_SCHEDULER" envDefault:"true"`

	// ScheduleStorePath is the JSON file persisting chatID -> the chat's cron jobs,
	// reloaded on startup so scheduled jobs survive a restart. When empty it
	// defaults to <APPROVED_DIRECTORY>/schedules.json (see ScheduleStoreFile), which
	// lives under the workspace data dir alongside the session/pending stores, never
	// inside a repo.
	ScheduleStorePath string `env:"SCHEDULE_STORE_PATH"`

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

	// EnableRichMessages toggles Bot API 10.1 rich rendering in the Telegram adapter
	// (Rich Markdown answers). Default true: answers are sent via sendRichMessage and
	// edited via editMessageText(rich_message=…). It can never break delivery — the
	// rich path falls back to the legacy MarkdownToHTML/plain rendering on any error,
	// and the adapter latches rich OFF for the process after repeated failures (so a
	// deployment where the live API does not yet serve these methods stops paying the
	// failed round-trip). Set ENABLE_RICH_MESSAGES=false to force the legacy path. VK
	// ignores it (no rich support). See docs/rich-messages-plan.md.
	EnableRichMessages bool `env:"ENABLE_RICH_MESSAGES" envDefault:"false"`

	// Team-loop knobs rendered into each workspace's CLAUDE.md (mirrors the
	// values the Python entrypoint substituted). Kept as strings since they are
	// substituted verbatim into the template.
	PrePRCycles    string `env:"PRE_PR_CYCLES" envDefault:"5"`
	PrReviewCycles string `env:"PR_REVIEW_CYCLES" envDefault:"10"`
	EnablePRReview string `env:"ENABLE_PR_REVIEW" envDefault:"false"`

	// AutoApproveScope controls the Phase-1 "confirm scope with the user" wait,
	// rendered into the workspace CLAUDE.md: off (always wait, the default) |
	// trivial | standard | all — the highest planner COMPLEXITY that proceeds
	// WITHOUT waiting for approval (the spec summary is still posted). Kept as a
	// string for verbatim template substitution; see AutoApproveScopeLevel.
	AutoApproveScope string `env:"AUTO_APPROVE_SCOPE" envDefault:"off"`

	// Post-run verification (core/verify): after a cleanly successful run the bot
	// re-runs the changed repos' OWN check gates (task/make/npm) from the Go side
	// and, on red, injects a fix-up run — bounded by POST_VERIFY_MAX_FIXES
	// consecutive rounds per chat. ENABLE_POST_VERIFY=false turns the whole pass
	// off; the timeout bounds ONE repo's gate command.
	EnablePostVerify         bool `env:"ENABLE_POST_VERIFY" envDefault:"true"`
	PostVerifyMaxFixes       int  `env:"POST_VERIFY_MAX_FIXES" envDefault:"2"`
	PostVerifyTimeoutSeconds int  `env:"POST_VERIFY_TIMEOUT_SECONDS" envDefault:"1800"`

	// /goal evaluator (core/goal): an INDEPENDENT fresh-session judge re-checks
	// the user's stated "done" criterion after every clean run, looping fix-ups
	// until achieved or GOAL_MAX_ATTEMPTS evaluation rounds. EVALUATOR_MODEL
	// overrides the judge's model (empty = the run model); the timeout / turn cap
	// bound one evaluator session. The store persists armed goals across restarts.
	GoalMaxAttempts        int    `env:"GOAL_MAX_ATTEMPTS" envDefault:"5"`
	EvaluatorModel         string `env:"EVALUATOR_MODEL"`
	GoalEvalTimeoutSeconds int    `env:"GOAL_EVAL_TIMEOUT_SECONDS" envDefault:"1800"`
	GoalEvalMaxTurns       int    `env:"GOAL_EVAL_MAX_TURNS" envDefault:"25"`
	GoalStorePath          string `env:"GOAL_STORE_PATH"`

	// CI watch (core/ciwatch): poll the git host for CI state on the duck/*
	// branches checked out in the workspaces; a red build injects a fix-up into
	// the owning chat, and (opt-in) a green PR is auto-merged. Needs GIT_TOKEN
	// plus either GITEA_API_URL or GIT_HOST=github.com (see CIWatchEnabled).
	EnableCIWatch   bool   `env:"ENABLE_CI_WATCH" envDefault:"false"`
	CIPollInterval  int    `env:"CI_POLL_INTERVAL" envDefault:"120"`
	EnableAutoMerge bool   `env:"ENABLE_AUTO_MERGE" envDefault:"false"`
	CIStatePath     string `env:"CI_STATE_PATH"`

	// AutoTaskMaxCostPerDay is the per-chat per-UTC-day USD ceiling for the
	// AUTONOMY-originated runs (CI fix-ups, verification fix-ups, goal-evaluator
	// rounds and their follow-ups). Reactive like the per-user cap; non-positive
	// disables the gate. Deliberately much smaller than CLAUDE_MAX_COST_PER_USER:
	// nobody is watching an autonomous loop overnight. Scheduled jobs are gated
	// separately (their creator's cost cap, see InjectScheduled).
	AutoTaskMaxCostPerDay float64 `env:"AUTO_TASK_MAX_COST_PER_DAY" envDefault:"20.0"`
	AutoBudgetStorePath   string  `env:"AUTO_BUDGET_STORE_PATH"`

	// Source paths for the shared team config baked into the image (see the
	// Dockerfile's /opt/duck layout). Rendered per chat by core/workspace.
	TeamTemplatePath string `env:"TEAM_TEMPLATE_PATH" envDefault:"/opt/duck/CLAUDE.workspace.md.tmpl"`
	TeamAgentsDir    string `env:"TEAM_AGENTS_DIR" envDefault:"/opt/duck/agents"`
	// TeamSkillsDir holds the Agent Skills baked into the image (each skill is a
	// <name>/SKILL.md subdirectory) copied into each workspace's .claude/skills/.
	// Empty or a missing dir disables skill rendering (the run just has no skills).
	TeamSkillsDir string `env:"TEAM_SKILLS_DIR" envDefault:"/opt/duck/skills"`

	// EnableContext7 wires the context7 MCP docs server into every run (an
	// .mcp.json is written at startup and passed via --mcp-config). context7 gives
	// the agent up-to-date, version-specific library/API docs so it stops guessing
	// outdated APIs — the highest-leverage accuracy boost for a coding agent. The
	// free tier needs no key; an unreachable server never breaks a run. Set
	// ENABLE_CONTEXT7=false to opt out.
	EnableContext7 bool `env:"ENABLE_CONTEXT7" envDefault:"true"`

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

// AIBackendName normalizes AI_BACKEND. The bool is false for unknown values; the
// returned backend is then the conservative Claude fallback.
func (c Config) AIBackendName() (string, bool) {
	switch strings.ToLower(strings.TrimSpace(c.AIBackend)) {
	case "", AIBackendClaude:
		return AIBackendClaude, true
	case AIBackendCodex:
		return AIBackendCodex, true
	case AIBackendOpenAICompat:
		return AIBackendOpenAICompat, true
	default:
		return AIBackendClaude, false
	}
}

// CodexAuthModeName normalizes CODEX_AUTH_MODE while preserving unknown values
// so ValidateCodex can reject them explicitly.
func (c Config) CodexAuthModeName() string {
	mode := strings.ToLower(strings.TrimSpace(c.CodexAuthMode))
	if mode == "" {
		return CodexAuthSubscription
	}
	return mode
}

// CodexAuthFile is the persisted login file used by Codex subscription auth.
func (c Config) CodexAuthFile() string {
	home := strings.TrimSpace(c.CodexHome)
	if home == "" {
		return ""
	}
	return filepath.Join(home, "auth.json")
}

// ValidateCodex checks the auth contract only when AI_BACKEND=codex. The
// subscription path accepts CODEX_ACCESS_TOKEN or a persisted auth.json under
// CODEX_HOME, and rejects CODEX_API_KEY to prevent accidental usage-based billing.
func (c Config) ValidateCodex() error {
	backend, ok := c.AIBackendName()
	if !ok || backend != AIBackendCodex {
		return nil
	}
	switch c.CodexAuthModeName() {
	case CodexAuthSubscription:
		if strings.TrimSpace(c.CodexAPIKey) != "" {
			return ErrCodexSubscriptionWithAPIKey
		}
		if !c.CodexRequireAuth {
			return nil
		}
		if strings.TrimSpace(c.CodexAccessToken) != "" {
			return nil
		}
		authFile := c.CodexAuthFile()
		if authFile != "" {
			if _, err := os.Stat(authFile); err == nil {
				return nil
			}
		}
		return ErrCodexSubscriptionAuthRequired
	case CodexAuthBilling:
		if strings.TrimSpace(c.CodexAPIKey) == "" {
			return ErrCodexBillingAPIKeyRequired
		}
		if !c.CodexBillingAck {
			return ErrCodexBillingAckRequired
		}
		return nil
	default:
		return ErrCodexUnknownAuthMode
	}
}

// OpenAICompatAuthModeName normalizes OPENAI_COMPAT_AUTH_MODE while preserving
// unknown values so ValidateOpenAICompat can reject them explicitly.
func (c Config) OpenAICompatAuthModeName() string {
	mode := strings.ToLower(strings.TrimSpace(c.OpenAICompatAuthMode))
	if mode == "" {
		return OpenAICompatAuthAPIKey
	}
	return mode
}

// OpenAICompatResolvedAPIKey returns the configured key, first from the direct
// env field, then from the env var named by OPENAI_COMPAT_API_KEY_ENV.
func (c Config) OpenAICompatResolvedAPIKey() string {
	if strings.TrimSpace(c.OpenAICompatAPIKey) != "" {
		return strings.TrimSpace(c.OpenAICompatAPIKey)
	}
	keyEnv := strings.TrimSpace(c.OpenAICompatAPIKeyEnv)
	if keyEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(keyEnv))
}

// OpenAICompatTimeout returns the HTTP timeout for the OpenAI-compatible provider.
func (c Config) OpenAICompatTimeout() time.Duration {
	if c.OpenAICompatTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.OpenAICompatTimeoutSeconds) * time.Second
}

// ValidateOpenAICompat checks the answer-only OpenAI-compatible provider config
// when AI_BACKEND=openai-compatible.
func (c Config) ValidateOpenAICompat() error {
	backend, ok := c.AIBackendName()
	if !ok || backend != AIBackendOpenAICompat {
		return nil
	}
	if strings.TrimSpace(c.OpenAICompatBaseURL) == "" {
		return ErrOpenAICompatBaseURLRequired
	}
	if strings.TrimSpace(c.OpenAICompatModel) == "" {
		return ErrOpenAICompatModelRequired
	}
	switch c.OpenAICompatAuthModeName() {
	case OpenAICompatAuthAPIKey:
		if c.OpenAICompatResolvedAPIKey() == "" {
			return ErrOpenAICompatAPIKeyRequired
		}
		if !c.OpenAICompatBillingAck {
			return ErrOpenAICompatBillingAckRequired
		}
		return nil
	default:
		return ErrOpenAICompatUnknownAuthMode
	}
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

// defaultShutdownDrain is the fallback graceful-drain window applied when
// SHUTDOWN_DRAIN_SECONDS is non-positive. Draining must never be fully disabled
// (a zero window would cancel every in-flight run immediately), so a bad/zero
// override falls back to the documented 45s default.
const defaultShutdownDrain = 45 * time.Second

// maxShutdownDrain caps the graceful-drain window. The shutdown budget is
// drain + the dispatcher's post-cancel grace (dispatch.postCancelGrace = 10s),
// and that total MUST stay below the Docker compose stop_grace_period (75s) so
// the process self-exits before SIGKILL. 60 + 10 = 70 < 75 leaves 5s of
// headroom. Raising stop_grace_period would require raising this cap to match.
const maxShutdownDrain = 60 * time.Second

// ShutdownDrain returns the graceful-drain window as a Duration: how long
// Shutdown waits for in-flight runs to finish before cancelling survivors.
//
// The window is clamped to [defaultShutdownDrain, maxShutdownDrain]. A
// non-positive SHUTDOWN_DRAIN_SECONDS falls back to defaultShutdownDrain so the
// drain is never disabled, and a value above the cap is clamped to
// maxShutdownDrain. The cap exists because the total shutdown budget is
// drain + the dispatcher's post-cancel grace (dispatch.postCancelGrace, 10s),
// and that sum must stay below the compose stop_grace_period (75s) so the
// process self-exits before SIGKILL — 60 + 10 = 70 < 75. Raising
// stop_grace_period would require raising maxShutdownDrain to match.
func (c Config) ShutdownDrain() time.Duration {
	if c.ShutdownDrainSeconds <= 0 {
		return defaultShutdownDrain
	}
	d := time.Duration(c.ShutdownDrainSeconds) * time.Second
	if d > maxShutdownDrain {
		return maxShutdownDrain
	}
	return d
}

// ShutdownDrainClamped reports whether the configured SHUTDOWN_DRAIN_SECONDS
// exceeded maxShutdownDrain and was clamped down by ShutdownDrain. Startup logs
// a one-line warning when this is true so an operator who set, say, 120 sees why
// the effective window is 60s.
func (c Config) ShutdownDrainClamped() bool {
	return time.Duration(c.ShutdownDrainSeconds)*time.Second > maxShutdownDrain
}

// PendingStoreFile returns the path to the JSON interrupted-run store, defaulting
// to <ApprovedDirectory>/pending.json when PENDING_STORE_PATH is unset so it
// lives under the workspace data dir alongside the session/cost stores, never
// inside any repo.
func (c Config) PendingStoreFile() string {
	if strings.TrimSpace(c.PendingStorePath) != "" {
		return c.PendingStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "pending.json")
}

// ScheduleStoreFile returns the path to the JSON cron-job store, defaulting to
// <ApprovedDirectory>/schedules.json when SCHEDULE_STORE_PATH is unset so it lives
// under the workspace data dir alongside the session/pending stores, never inside
// any repo.
func (c Config) ScheduleStoreFile() string {
	if strings.TrimSpace(c.ScheduleStorePath) != "" {
		return c.ScheduleStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "schedules.json")
}

// SchedulerEnabled reports whether the /schedule command and its background cron
// scheduler are active (ENABLE_SCHEDULER=true). When false the background side is
// a no-op (no store, no goroutine); /schedule stays bot-owned and replies with a
// disabled notice instead of running.
func (c Config) SchedulerEnabled() bool {
	return c.EnableScheduler
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

// UsingSubscription reports whether the bot is authenticated with a Claude
// subscription (OAuth) rather than a billed API key. It is true only when
// CLAUDE_CODE_OAUTH_TOKEN is set AND ANTHROPIC_API_KEY is empty (both after
// TrimSpace, matching every other token check here).
//
// This mirrors the documented auth precedence (see adapters/telegram/.env.example):
// when BOTH tokens are set the API key takes precedence and bills, so that case is
// NOT a subscription. On a subscription, total_cost_usd is a notional
// token-dollar-equivalent rather than real spend, which is why the cost cap is
// disabled in that mode (see CostCapEnabled).
func (c Config) UsingSubscription() bool {
	return strings.TrimSpace(c.ClaudeCodeOAuthToken) != "" && strings.TrimSpace(c.AnthropicAPIKey) == ""
}

// CostCapEnabled reports whether the cumulative per-user cost cap is active. It is
// FALSE on a Claude subscription (UsingSubscription): there total_cost_usd is a
// notional token-dollar-equivalent, not billed money, so capping it would deny
// users over an amount that was never spent. Otherwise it follows the configured
// cap: a positive CLAUDE_MAX_COST_PER_USER enables the gate, a non-positive value
// disables it (every request is allowed regardless of accumulated spend).
func (c Config) CostCapEnabled() bool {
	if c.UsingSubscription() {
		return false
	}
	return c.ClaudeMaxCostPerUser > 0
}

// EffectiveCostCapUSD is the cumulative per-user cap to feed into
// chat.GuardConfig.CostCapUSD. It returns 0 when the cap is disabled (including the
// subscription case, see CostCapEnabled) and otherwise the configured
// CLAUDE_MAX_COST_PER_USER. The "cap <= 0 disables" contract in core/cost then does
// the rest. This is the single source of truth so the startup log and the guard
// wiring can never diverge.
func (c Config) EffectiveCostCapUSD() float64 {
	if !c.CostCapEnabled() {
		return 0
	}
	return c.ClaudeMaxCostPerUser
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

// autoApproveLevels is the set of accepted AUTO_APPROVE_SCOPE values.
var autoApproveLevels = map[string]struct{}{
	"off": {}, "trivial": {}, "standard": {}, "all": {},
}

// AutoApproveScopeLevel normalizes AUTO_APPROVE_SCOPE for template rendering:
// lowercased, and any unrecognized value falls back to "off" (fail-safe: a typo
// must never silently skip the human approval gate).
func (c Config) AutoApproveScopeLevel() string {
	v := strings.ToLower(strings.TrimSpace(c.AutoApproveScope))
	if _, ok := autoApproveLevels[v]; ok {
		return v
	}
	return "off"
}

// PostVerifyTimeout returns the per-repo gate-command timeout as a Duration; a
// non-positive value falls back to the verifier's built-in default.
func (c Config) PostVerifyTimeout() time.Duration {
	if c.PostVerifyTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.PostVerifyTimeoutSeconds) * time.Second
}

// GoalEvalTimeout returns the evaluator-session deadline as a Duration, or 0
// when no timeout is configured.
func (c Config) GoalEvalTimeout() time.Duration {
	if c.GoalEvalTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.GoalEvalTimeoutSeconds) * time.Second
}

// GoalStoreFile returns the path to the JSON goal store, defaulting to
// <ApprovedDirectory>/goals.json so it lives under the workspace data dir
// alongside the session/cost stores, never inside a repo.
func (c Config) GoalStoreFile() string {
	if strings.TrimSpace(c.GoalStorePath) != "" {
		return c.GoalStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "goals.json")
}

// CIStateFile returns the path to the JSON CI-watch dedupe store, defaulting to
// <ApprovedDirectory>/ci_state.json.
func (c Config) CIStateFile() string {
	if strings.TrimSpace(c.CIStatePath) != "" {
		return c.CIStatePath
	}
	return filepath.Join(c.ApprovedDirectory, "ci_state.json")
}

// AutoBudgetStoreFile returns the path to the JSON autonomy-budget store,
// defaulting to <ApprovedDirectory>/auto_budget.json.
func (c Config) AutoBudgetStoreFile() string {
	if strings.TrimSpace(c.AutoBudgetStorePath) != "" {
		return c.AutoBudgetStorePath
	}
	return filepath.Join(c.ApprovedDirectory, "auto_budget.json")
}

// minCIPollInterval floors the CI poll period, mirroring the Gitea poller.
const minCIPollInterval = 30 * time.Second

// CIPollDuration returns the CI poll period as a Duration, floored to 30s, or 0
// when the configured interval is not positive.
func (c Config) CIPollDuration() time.Duration {
	if c.CIPollInterval <= 0 {
		return 0
	}
	d := time.Duration(c.CIPollInterval) * time.Second
	if d < minCIPollInterval {
		return minCIPollInterval
	}
	return d
}

// CIWatchGitHub reports whether the CI watch should speak the GitHub API (the
// deployment's git host is github.com); otherwise it uses the Gitea API URL.
func (c Config) CIWatchGitHub() bool {
	return strings.EqualFold(strings.TrimSpace(c.GitHost), gitHubHost)
}

// CIWatchEnabled reports whether the CI watch loop should run: the feature flag
// is on, a token is set, a positive interval is configured, and the deployment
// has a host API to poll (github.com, or a Gitea API URL). Unlike the PR-comment
// poller it does NOT require ENABLE_PR_REVIEW — reacting to a red build is
// useful even when the on-PR review loop is off.
func (c Config) CIWatchEnabled() bool {
	if !c.EnableCIWatch || c.GitToken == "" || c.CIPollInterval <= 0 {
		return false
	}
	return c.CIWatchGitHub() || c.GiteaAPIURL != ""
}
