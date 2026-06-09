//nolint:testpackage // intentionally whitebox to test unexported config parsing internals
package config

import (
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantErr    bool
		wantUsers  []int64
		wantLevel  slog.Level
		checkLevel bool
	}{
		{
			name: "happy path",
			env: map[string]string{
				"TELEGRAM_BOT_TOKEN":    "token",
				"TELEGRAM_BOT_USERNAME": "duckbot",
				"ALLOWED_USERS":         "1,2",
			},
			wantUsers: []int64{1, 2},
		},
		{
			name: "empty allowed users",
			env: map[string]string{
				"TELEGRAM_BOT_TOKEN":    "token",
				"TELEGRAM_BOT_USERNAME": "duckbot",
				"ALLOWED_USERS":         "",
			},
			wantUsers: nil,
		},
		{
			name: "missing telegram token still loads (validated per-binary)",
			env: map[string]string{
				"TELEGRAM_BOT_USERNAME": "duckbot",
			},
			wantUsers: nil,
		},
		{
			name: "log level mapping",
			env: map[string]string{
				"TELEGRAM_BOT_TOKEN": "token",
				"LOG_LEVEL":          "debug",
			},
			wantLevel:  slog.LevelDebug,
			checkLevel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}

			if len(tt.wantUsers) != len(cfg.AllowedUsers) {
				t.Fatalf("AllowedUsers = %v, want %v", cfg.AllowedUsers, tt.wantUsers)
			}
			for i := range tt.wantUsers {
				if cfg.AllowedUsers[i] != tt.wantUsers[i] {
					t.Fatalf("AllowedUsers = %v, want %v", cfg.AllowedUsers, tt.wantUsers)
				}
			}

			if tt.checkLevel && cfg.SlogLevel() != tt.wantLevel {
				t.Fatalf("SlogLevel() = %v, want %v", cfg.SlogLevel(), tt.wantLevel)
			}
		})
	}
}

func TestTeamConfigDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.PrePRCycles != "5" || cfg.PrReviewCycles != "10" || cfg.EnablePRReview != "false" {
		t.Fatalf("team-loop defaults = %q/%q/%q, want 5/10/false",
			cfg.PrePRCycles, cfg.PrReviewCycles, cfg.EnablePRReview)
	}
	if cfg.TeamTemplatePath != "/opt/duck/CLAUDE.workspace.md.tmpl" || cfg.TeamAgentsDir != "/opt/duck/agents" {
		t.Fatalf("team source defaults = %q/%q", cfg.TeamTemplatePath, cfg.TeamAgentsDir)
	}
	// REQUIRE_GROUP_MENTION defaults to false to match the Python adapter's
	// .env.example / mention_gate.py (answer every group message by default).
	if cfg.RequireGroupMention {
		t.Fatalf("RequireGroupMention default = true, want false (match Python)")
	}
}

func TestRequireGroupMention(t *testing.T) {
	for _, tt := range []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"", false},
	} {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("TELEGRAM_BOT_TOKEN", "token")
			t.Setenv("REQUIRE_GROUP_MENTION", tt.val)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.RequireGroupMention != tt.want {
				t.Fatalf("RequireGroupMention(%q) = %v, want %v", tt.val, cfg.RequireGroupMention, tt.want)
			}
		})
	}
}

func TestSlogLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"DEBUG", slog.LevelDebug},
		{"debug", slog.LevelDebug},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo},
	}
	for _, tt := range tests {
		c := Config{LogLevel: tt.in}
		if got := c.SlogLevel(); got != tt.want {
			t.Errorf("SlogLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestClaudeTimeout(t *testing.T) {
	for _, tt := range []struct {
		secs int
		want time.Duration
	}{
		{0, 0},
		{-5, 0},
		{30, 30 * time.Second},
	} {
		c := Config{ClaudeTimeoutSeconds: tt.secs}
		if got := c.ClaudeTimeout(); got != tt.want {
			t.Errorf("ClaudeTimeout(%d) = %v, want %v", tt.secs, got, tt.want)
		}
	}
}

func TestSessionStoreFile(t *testing.T) {
	// Explicit path wins.
	c := Config{SessionStorePath: "/var/lib/duck/s.json", ApprovedDirectory: "/workspace"}
	if got := c.SessionStoreFile(); got != "/var/lib/duck/s.json" {
		t.Errorf("SessionStoreFile() = %q, want explicit path", got)
	}
	// Otherwise derive from APPROVED_DIRECTORY.
	c = Config{ApprovedDirectory: "/workspace"}
	if got := c.SessionStoreFile(); got != "/workspace/sessions.json" {
		t.Errorf("SessionStoreFile() = %q, want /workspace/sessions.json", got)
	}
}

func TestPRReviewEnabled(t *testing.T) {
	for _, tt := range []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"nonsense", false},
	} {
		c := Config{EnablePRReview: tt.val}
		if got := c.PRReviewEnabled(); got != tt.want {
			t.Errorf("PRReviewEnabled(%q) = %v, want %v", tt.val, got, tt.want)
		}
	}
}

func TestGiteaPollDuration(t *testing.T) {
	for _, tt := range []struct {
		secs int
		want time.Duration
	}{
		{0, 0},
		{-1, 0},
		{10, 30 * time.Second}, // floored
		{30, 30 * time.Second},
		{90, 90 * time.Second},
	} {
		c := Config{GiteaPollInterval: tt.secs}
		if got := c.GiteaPollDuration(); got != tt.want {
			t.Errorf("GiteaPollDuration(%d) = %v, want %v", tt.secs, got, tt.want)
		}
	}
}

func TestPollerEnabled(t *testing.T) {
	base := Config{
		EnablePRReview:    "true",
		GiteaAPIURL:       "https://git.example.com/api/v1",
		GitToken:          "tok",
		GiteaPollInterval: 90,
	}
	if !base.PollerEnabled() {
		t.Errorf("PollerEnabled() = false, want true when fully configured")
	}
	for _, tt := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"review off", func(c *Config) { c.EnablePRReview = "false" }},
		{"no api url", func(c *Config) { c.GiteaAPIURL = "" }},
		{"no token", func(c *Config) { c.GitToken = "" }},
		{"zero interval", func(c *Config) { c.GiteaPollInterval = 0 }},
	} {
		c := base
		tt.mutate(&c)
		if c.PollerEnabled() {
			t.Errorf("PollerEnabled() = true for %q, want false", tt.name)
		}
	}
}

func TestGitDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.GitScheme != "https" {
		t.Errorf("GitScheme default = %q, want https", cfg.GitScheme)
	}
	if cfg.GitAuthorName != "AI Team" || cfg.GitAuthorEmail != "ai@example.com" {
		t.Errorf("git author defaults = %q/%q", cfg.GitAuthorName, cfg.GitAuthorEmail)
	}
	if cfg.GiteaPollInterval != 90 {
		t.Errorf("GiteaPollInterval default = %d, want 90", cfg.GiteaPollInterval)
	}
}

func TestVoiceDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.EnableVoiceMessages {
		t.Errorf("EnableVoiceMessages default = true, want false")
	}
	if cfg.VoiceProvider != "mistral" {
		t.Errorf("VoiceProvider default = %q, want mistral", cfg.VoiceProvider)
	}
	if cfg.VoiceMistralModel != "voxtral-mini-latest" {
		t.Errorf("VoiceMistralModel default = %q, want voxtral-mini-latest", cfg.VoiceMistralModel)
	}
	if cfg.VoiceOpenAIModel != "whisper-1" {
		t.Errorf("VoiceOpenAIModel default = %q, want whisper-1", cfg.VoiceOpenAIModel)
	}
}

func TestMaxUploadBytesDefault(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.MaxUploadBytes != 20971520 {
		t.Errorf("MaxUploadBytes default = %d, want 20971520 (20 MiB)", cfg.MaxUploadBytes)
	}
}

func TestMaxOutboxDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.MaxOutboxBytes != 20971520 {
		t.Errorf("MaxOutboxBytes default = %d, want 20971520 (20 MiB)", cfg.MaxOutboxBytes)
	}
	if cfg.MaxOutboxFiles != 10 {
		t.Errorf("MaxOutboxFiles default = %d, want 10", cfg.MaxOutboxFiles)
	}
}

func TestEnableVoiceMessagesParses(t *testing.T) {
	for _, tt := range []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"false", false},
	} {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("TELEGRAM_BOT_TOKEN", "token")
			t.Setenv("ENABLE_VOICE_MESSAGES", tt.val)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.EnableVoiceMessages != tt.want {
				t.Errorf("EnableVoiceMessages(%q) = %v, want %v", tt.val, cfg.EnableVoiceMessages, tt.want)
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	c := Config{AllowedUsers: []int64{10, 20}}
	if !c.IsAllowed(10) {
		t.Errorf("IsAllowed(10) = false, want true")
	}
	if c.IsAllowed(99) {
		t.Errorf("IsAllowed(99) = true, want false")
	}
}

func TestValidateTelegram(t *testing.T) {
	if err := (Config{TelegramBotToken: "token"}).ValidateTelegram(); err != nil {
		t.Errorf("ValidateTelegram() with token = %v, want nil", err)
	}
	if err := (Config{TelegramBotToken: "  "}).ValidateTelegram(); err == nil {
		t.Error("ValidateTelegram() with blank token = nil, want error")
	}
	if err := (Config{}).ValidateTelegram(); err == nil {
		t.Error("ValidateTelegram() with no token = nil, want error")
	}
}

func TestVKConfigParses(t *testing.T) {
	t.Setenv("VK_BOT_TOKEN", "vk-community-token")
	t.Setenv("VK_GROUP_ID", "123456")
	t.Setenv("VK_ALLOWED_USERS", "10,20,30")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.VKBotToken != "vk-community-token" {
		t.Errorf("VKBotToken = %q, want vk-community-token", cfg.VKBotToken)
	}
	if cfg.VKGroupID != 123456 {
		t.Errorf("VKGroupID = %d, want 123456", cfg.VKGroupID)
	}
	want := []int64{10, 20, 30}
	if len(cfg.VKAllowedUsers) != len(want) {
		t.Fatalf("VKAllowedUsers = %v, want %v", cfg.VKAllowedUsers, want)
	}
	for i := range want {
		if cfg.VKAllowedUsers[i] != want[i] {
			t.Fatalf("VKAllowedUsers = %v, want %v", cfg.VKAllowedUsers, want)
		}
	}
}

func TestValidateVK(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{"ok", Config{VKBotToken: "t", VKGroupID: 1}, nil},
		{"missing token", Config{VKGroupID: 1}, ErrMissingVKToken},
		{"blank token", Config{VKBotToken: "  ", VKGroupID: 1}, ErrMissingVKToken},
		{"missing group", Config{VKBotToken: "t"}, ErrMissingVKGroupID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.ValidateVK()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateVK() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateVK() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsVKAllowed(t *testing.T) {
	c := Config{VKAllowedUsers: []int64{10, 20}}
	if !c.IsVKAllowed(10) {
		t.Errorf("IsVKAllowed(10) = false, want true")
	}
	if c.IsVKAllowed(99) {
		t.Errorf("IsVKAllowed(99) = true, want false")
	}
	// The VK list is independent of the Telegram one.
	c = Config{AllowedUsers: []int64{5}}
	if c.IsVKAllowed(5) {
		t.Errorf("IsVKAllowed(5) = true on a Telegram-only allow-list, want false")
	}
}

func TestGuardrailDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.RateLimitRequests != 30 {
		t.Errorf("RateLimitRequests default = %d, want 30", cfg.RateLimitRequests)
	}
	if cfg.RateLimitWindowSecs != 60 {
		t.Errorf("RateLimitWindowSecs default = %d, want 60", cfg.RateLimitWindowSecs)
	}
	if cfg.ClaudeMaxCostPerUser != 1000.0 {
		t.Errorf("ClaudeMaxCostPerUser default = %v, want 1000.0", cfg.ClaudeMaxCostPerUser)
	}
}

func TestRateLimitWindow(t *testing.T) {
	for _, tt := range []struct {
		secs int
		want time.Duration
	}{
		{0, 0},
		{-5, 0},
		{60, 60 * time.Second},
	} {
		c := Config{RateLimitWindowSecs: tt.secs}
		if got := c.RateLimitWindow(); got != tt.want {
			t.Errorf("RateLimitWindow(%d) = %v, want %v", tt.secs, got, tt.want)
		}
	}
}

func TestRateLimitEnabled(t *testing.T) {
	for _, tt := range []struct {
		reqs int
		secs int
		want bool
	}{
		{30, 60, true},
		{0, 60, false},
		{30, 0, false},
		{-1, 60, false},
		{30, -1, false},
	} {
		c := Config{RateLimitRequests: tt.reqs, RateLimitWindowSecs: tt.secs}
		if got := c.RateLimitEnabled(); got != tt.want {
			t.Errorf("RateLimitEnabled(reqs=%d,secs=%d) = %v, want %v", tt.reqs, tt.secs, got, tt.want)
		}
	}
}

func TestCostCapEnabled(t *testing.T) {
	for _, tt := range []struct {
		cap  float64
		want bool
	}{
		{1000.0, true},
		{0, false},
		{-1, false},
	} {
		c := Config{ClaudeMaxCostPerUser: tt.cap}
		if got := c.CostCapEnabled(); got != tt.want {
			t.Errorf("CostCapEnabled(%v) = %v, want %v", tt.cap, got, tt.want)
		}
	}
}

func TestCostStoreFile(t *testing.T) {
	// Explicit path wins.
	c := Config{CostStorePath: "/var/lib/duck/c.json", ApprovedDirectory: "/workspace"}
	if got := c.CostStoreFile(); got != "/var/lib/duck/c.json" {
		t.Errorf("CostStoreFile() = %q, want explicit path", got)
	}
	// Otherwise derive from APPROVED_DIRECTORY.
	c = Config{ApprovedDirectory: "/workspace"}
	if got := c.CostStoreFile(); got != "/workspace/costs.json" {
		t.Errorf("CostStoreFile() = %q, want /workspace/costs.json", got)
	}
}

func TestStarNudgeStoreFile(t *testing.T) {
	// Explicit path wins.
	c := Config{StarNudgeStorePath: "/var/lib/duck/s.json", ApprovedDirectory: "/workspace"}
	if got := c.StarNudgeStoreFile(); got != "/var/lib/duck/s.json" {
		t.Errorf("StarNudgeStoreFile() = %q, want explicit path", got)
	}
	// Otherwise derive from APPROVED_DIRECTORY.
	c = Config{ApprovedDirectory: "/workspace"}
	if got := c.StarNudgeStoreFile(); got != "/workspace/star_nudge.json" {
		t.Errorf("StarNudgeStoreFile() = %q, want /workspace/star_nudge.json", got)
	}
}

func TestStarNudgeRepoParts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		repo      string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{name: "valid", repo: "duckbugio/flock", wantOwner: "duckbugio", wantRepo: "flock", wantOK: true},
		{name: "trims spaces", repo: " duckbugio / flock ", wantOwner: "duckbugio", wantRepo: "flock", wantOK: true},
		{name: "empty", repo: "", wantOK: false},
		{name: "no slash", repo: "flock", wantOK: false},
		{name: "too many parts", repo: "a/b/c", wantOK: false},
		{name: "blank owner", repo: "/flock", wantOK: false},
		{name: "blank repo", repo: "duckbugio/", wantOK: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{StarNudgeRepo: tt.repo}
			owner, repo, ok := c.StarNudgeRepoParts()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (owner != tt.wantOwner || repo != tt.wantRepo) {
				t.Errorf("parts = %q/%q, want %q/%q", owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestStarNudgeEnabled(t *testing.T) {
	base := Config{GitHost: "github.com", GitToken: "tok", StarNudgeRepo: "duckbugio/flock"}
	if !base.StarNudgeEnabled() {
		t.Errorf("StarNudgeEnabled() = false, want true when fully configured")
	}
	// Host match is case-insensitive.
	c := base
	c.GitHost = "GitHub.com"
	if !c.StarNudgeEnabled() {
		t.Errorf("StarNudgeEnabled() = false for case-variant host, want true")
	}
	for _, tt := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"gitea host", func(c *Config) { c.GitHost = "git.example.com" }},
		{"empty host", func(c *Config) { c.GitHost = "" }},
		{"no token", func(c *Config) { c.GitToken = "" }},
		{"bad repo", func(c *Config) { c.StarNudgeRepo = "flock" }},
	} {
		c := base
		tt.mutate(&c)
		if c.StarNudgeEnabled() {
			t.Errorf("StarNudgeEnabled() = true for %q, want false", tt.name)
		}
	}
}
