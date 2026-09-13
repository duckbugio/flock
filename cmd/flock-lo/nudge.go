package main

import (
	"log/slog"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/ghstar"
	"github.com/duckbugio/flock/core/nudge"
	"github.com/duckbugio/flock/internal/config"
)

// buildStarNudge enables account actions only when both the operator's nudge
// setting and this LO deployment's keyboard capability are enabled.
func buildStarNudge(cfg config.Config, logger *slog.Logger) chat.StarNudgeConfig {
	if !cfg.LOEnableKeyboards || !cfg.StarNudgeEnabled() {
		return chat.StarNudgeConfig{}
	}
	owner, repo, ok := cfg.StarNudgeRepoParts()
	if !ok {
		return chat.StarNudgeConfig{}
	}
	store, err := nudge.Open(cfg.StarNudgeStoreFile())
	if err != nil {
		logger.Error("open star-nudge store; nudge disabled", "path", cfg.StarNudgeStoreFile(), "error", err)
		return chat.StarNudgeConfig{}
	}
	return chat.StarNudgeConfig{
		Enabled: true, Owner: owner, Repo: repo,
		Client: ghstar.New(ghstar.Config{Token: cfg.GitToken, Logger: logger}), Store: store,
	}
}
