// Command flock-lo connects LO bot messages to the shared Flock agent runtime.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // Keep scheduler timezones available in minimal containers.

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/gitsetup"
	"github.com/duckbugio/flock/core/pending"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/core/schedule"
	"github.com/duckbugio/flock/core/session"
	"github.com/duckbugio/flock/core/workspace"
	"github.com/duckbugio/flock/internal/airunner"
	"github.com/duckbugio/flock/internal/autonomy"
	"github.com/duckbugio/flock/internal/config"
)

const workspaceMode = 0o700

func main() { os.Exit(run()) }

func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	if err := cfg.ValidateLO(); err != nil {
		slog.Error("invalid LO config", "error", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)
	// A Telegram/VK chat with the same numeric ID must never reuse an LO workspace.
	cfg.ApprovedDirectory = filepath.Join(cfg.ApprovedDirectory, "lo")
	if err := os.MkdirAll(cfg.ApprovedDirectory, workspaceMode); err != nil {
		logger.Error("create LO workspace", "error", err)
		return 1
	}
	api, err := lo.NewClient(cfg.LOAPIURL, cfg.LOBotToken, nil)
	if err != nil {
		logger.Error("configure LO client", "error", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	self, err := api.GetMe(ctx)
	if err != nil {
		logger.Error("authenticate LO bot", "error", err)
		return 1
	}
	if self.Username == "" {
		logger.Warn("LO bot has no username; group mentions cannot match; use private chats or set REQUIRE_GROUP_MENTION=false")
	}
	if err := api.CheckPolling(ctx); err != nil {
		logger.Error("LO polling unavailable", "error", err)
		return 1
	}
	if cfg.LORegisterCommands {
		if err := api.SetCommands(ctx); err != nil {
			logger.Warn("LO command menu unavailable; text commands still work", "error", err)
		}
	}
	runner, opts, provider, err := airunner.Build(cfg)
	if err != nil {
		logger.Error("configure AI backend", "error", err)
		return 1
	}
	logger.Info("AI backend configured", "provider", provider.Name)
	if err := gitsetup.Apply(ctx, gitsetup.Config{
		Host:        cfg.GitHost,
		Scheme:      cfg.GitScheme,
		AuthorName:  cfg.GitAuthorName,
		AuthorEmail: cfg.GitAuthorEmail,
		HasToken:    cfg.GitToken != "",
	}); err != nil {
		logger.Warn("git setup", "error", err)
	}
	if provider.Name == config.AIBackendClaude {
		servers := map[string]claude.MCPServer{}
		if cfg.EnableContext7 {
			servers["context7"] = claude.MCPServer{URL: claude.Context7URL}
		}
		if cfg.DuckBugMCPEnabled() {
			servers["duckbug"] = claude.MCPServer{URL: cfg.DuckBugMCPURL, BearerToken: cfg.DuckBugMCPToken}
		}
		path := filepath.Join(cfg.ApprovedDirectory, ".flock-mcp.json")
		if ok, err := claude.WriteMCPConfig(path, servers); err != nil {
			logger.Warn("write MCP config", "error", err)
		} else if ok {
			opts.MCPConfig = path
		}
	}
	ws := &workspace.Renderer{
		// Promises about files follow the flag: with documents off the workspace instructions
		// tell the agent not to offer them, because the transport cannot deliver.
		FileDeliveryDisabled: !cfg.LOEnableDocuments,
		AutoApproveScope:     cfg.AutoApproveScopeLevel(),
		BaseDir:              cfg.ApprovedDirectory,
		TemplatePath:         cfg.TeamTemplatePath,
		AgentsDir:            cfg.TeamAgentsDir,
		SkillsDir:            cfg.TeamSkillsDir,
		PrePRCycles:          cfg.PrePRCycles,
		PrReviewCycles:       cfg.PrReviewCycles,
		EnablePRReview:       cfg.EnablePRReview,
		GitHost:              cfg.GitHost,
	}
	sessions, err := session.Open(cfg.SessionStoreFile())
	if err != nil {
		logger.Error("open sessions", "error", err)
		return 1
	}
	costs, err := cost.Open(cfg.CostStoreFile())
	if err != nil {
		logger.Error("open costs", "error", err)
		return 1
	}
	pendings, err := pending.Open(cfg.PendingStoreFile())
	if err != nil {
		logger.Error("open interrupted-run queue", "error", err)
		return 1
	}
	limiter := ratelimit.New(cfg.RateLimitRequests, cfg.RateLimitWindow())
	dispatcher := dispatch.New(cfg.MaxConcurrentChatRuns)
	if cfg.ShutdownDrainClamped() {
		logger.Warn("SHUTDOWN_DRAIN_SECONDS above cap; clamped",
			"configured_seconds", cfg.ShutdownDrainSeconds, "effective", cfg.ShutdownDrain())
	}
	defer func() {
		drain, stop := context.WithTimeout(context.Background(), cfg.ShutdownDrain())
		defer stop()
		if err := dispatcher.Shutdown(drain); err != nil {
			logger.Warn("dispatcher drain", "error", err)
		}
	}()
	transport := lo.NewTransport(api, cfg.LOEnableDrafts).WithDocuments(cfg.LOEnableDocuments)
	postRun := autonomy.Build(cfg, logger)
	// The outbox sweep is what turns an agent's file into a delivery. Always constructed, as
	// in the Telegram and VK binaries: the core already skips the sweep when the transport
	// reports it cannot send documents, so the flag is read in exactly one place.
	outbox := chat.NewSweeper(ws, cfg.MaxOutboxBytes, cfg.MaxOutboxFiles, logger)
	svc := chat.New(chat.Config{
		Runner:     runner,
		Transport:  transport,
		Dispatcher: dispatcher,
		Workspace:  ws,
		Sessions:   sessions,
		Pending:    pendings,
		Outbox:     outbox,
		Costs:      costs,
		CostCapUSD: cfg.EffectiveCostCapUSD(),
		PostRun:    postRun,
		Opts:       opts,
		Timeout:    cfg.ClaudeTimeout(),
		RetryAfter: lo.RetryAfter,
		Logger:     logger,
	})
	// Replay before any background or user submissions can enter the dispatcher.
	resumePending(ctx, transport, svc, pendings, logger)
	autonomy.StartFollowups(ctx, svc, postRun, logger)
	startReviewPoller(ctx, cfg, svc, logger)
	startCIWatch(ctx, cfg, transport, svc, logger)
	scheduler := startScheduler(ctx, cfg, svc, logger)
	// Inbound photo downloads. Always constructed: the uploader writes into the per-chat
	// uploads directory, a sibling of the cloned repositories, so user media is not
	// swept into a commit. Missing media bytes get a specific explanatory notice.
	uploads := lo.NewUploader(api, ws, cfg.MaxUploadBytes, logger)
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service:        svc,
		Client:         api,
		Transport:      transport,
		Username:       self.Username,
		BotID:          self.ID,
		IsAllowed:      cfg.IsLOAllowed,
		RequireMention: cfg.RequireGroupMention,
		Scheduler:      scheduler,
		Uploads:        uploads,
		Voice:          buildVoice(cfg, api, logger),
		Logger:         logger,
		Guards: func(id int64) (bool, string) {
			return chat.CheckGuards(limiter, costs, chat.GuardConfig{CostCapUSD: cfg.EffectiveCostCapUSD()}, id)
		},
	})
	logger.Info("starting LO adapter", "bot_id", self.ID, "drafts", cfg.LOEnableDrafts, "workspace", cfg.ApprovedDirectory)
	logger.Info("LO compatibility: text, photos, documents and optional voice; /stop replaces buttons; native replies disabled",
		"documents", cfg.LOEnableDocuments)
	if err := receiver.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("LO adapter stopped", "error", err)
		return 1
	}
	return 0
}

func startScheduler(ctx context.Context, cfg config.Config, svc *chat.Service, logger *slog.Logger) *schedule.Manager {
	if !cfg.SchedulerEnabled() {
		return nil
	}
	store, err := schedule.Open(cfg.ScheduleStoreFile())
	if err != nil {
		logger.Error("open schedules", "error", err)
		return nil
	}
	manager := schedule.NewManager(store, svc.InjectScheduled, cfg.IsLOAllowed, time.Now, logger)
	go func() {
		if err := manager.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("LO scheduler stopped", "error", err)
		}
	}()
	return manager
}
