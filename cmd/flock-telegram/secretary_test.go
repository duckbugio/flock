package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/core/session"
	"github.com/duckbugio/flock/core/workspace"
	"github.com/duckbugio/flock/internal/config"
)

type secretaryFakeRunner struct {
	calls   int
	onRun   func()
	events  []agent.Event
	prompts []string
	options []agent.Options
}

func (r *secretaryFakeRunner) Run(_ context.Context, prompt string, opts agent.Options) (<-chan agent.Event, error) {
	r.calls++
	r.prompts = append(r.prompts, prompt)
	r.options = append(r.options, opts)
	if r.onRun != nil {
		r.onRun()
	}
	events := r.events
	if events == nil {
		events = []agent.Event{{Type: agent.Result, Result: &agent.RunResult{Text: "Draft reply"}}}
	}
	ch := make(chan agent.Event, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type secretaryFakeAPI struct {
	connection models.BusinessConnection
	gets       int
	sends      []*bot.SendMessageParams
	edits      []*bot.EditMessageTextParams
	callbacks  []*bot.AnswerCallbackQueryParams
	onSend     func(*bot.SendMessageParams)
	onGet      func()
}

func (f *secretaryFakeAPI) GetBusinessConnection(
	_ context.Context, _ *bot.GetBusinessConnectionParams,
) (*models.BusinessConnection, error) {
	f.gets++
	c := f.connection
	if f.onGet != nil {
		f.onGet()
	}
	return &c, nil
}

func (f *secretaryFakeAPI) SendMessage(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	f.sends = append(f.sends, p)
	if f.onSend != nil {
		f.onSend(p)
	}
	return &models.Message{ID: len(f.sends), Chat: models.Chat{ID: p.ChatID.(int64)}, Text: p.Text}, nil
}

func (f *secretaryFakeAPI) EditMessageText(_ context.Context, p *bot.EditMessageTextParams) (*models.Message, error) {
	f.edits = append(f.edits, p)
	return &models.Message{}, nil
}

func (f *secretaryFakeAPI) AnswerCallbackQuery(_ context.Context, p *bot.AnswerCallbackQueryParams) (bool, error) {
	f.callbacks = append(f.callbacks, p)
	return true, nil
}

func testSecretary(t *testing.T, mode string) (*secretaryManager, *secretaryFakeRunner, *secretaryFakeAPI) {
	t.Helper()
	runner := &secretaryFakeRunner{}
	base := t.TempDir()
	template := filepath.Join(base, "template.md")
	if err := os.WriteFile(template, []byte("# Flock agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(base, "agents")
	if err := os.MkdirAll(agents, 0o750); err != nil {
		t.Fatal(err)
	}
	sessions, err := session.Open(filepath.Join(base, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := &secretaryManager{
		mode: mode, runner: runner, path: filepath.Join(base, "secretary.json"),
		workspace: &workspace.Renderer{BaseDir: base, TemplatePath: template, AgentsDir: agents},
		sessions:  sessions,
		allow:     func(id int64) bool { return id == 10 },
		limit:     ratelimit.New(30, time.Minute), sem: make(chan struct{}, 4),
		slots:  make(chan struct{}, secretaryQueueCapacity),
		state:  secretaryState{Seen: map[string]int64{}, Invalid: map[string]int64{}, Pending: map[string]secretaryPending{}},
		lanes:  make(map[string]*secretaryLane),
		active: make(map[string]secretaryActiveRun),
	}
	api := &secretaryFakeAPI{connection: models.BusinessConnection{
		ID: "conn", User: models.User{ID: 10}, UserChatID: 100, IsEnabled: true,
		Rights: &models.BusinessBotRights{CanReply: true},
	}}
	return m, runner, api
}

func incomingSecretaryMessage() *models.Message {
	return &models.Message{
		ID: 5, BusinessConnectionID: "conn", From: &models.User{ID: 20, FirstName: "Alex"},
		Chat: models.Chat{ID: 200, Type: models.ChatTypePrivate}, Text: "Hello",
	}
}

func TestSecretaryOtherProviderIsUnavailable(t *testing.T) {
	runner := &secretaryFakeRunner{}
	approved := t.TempDir()
	m, err := newSecretaryManager(config.Config{
		SecretaryMode: config.SecretaryModeAuto, ApprovedDirectory: approved,
	}, secretaryRuntime{runner: runner, opts: agent.Options{
		Model: "selected-model", MCPConfig: "/workspace/mcp.json", SessionID: "old-session",
		Workdir: "/workspace/project", MaxTurns: 40, Effort: "ultracode",
	}, providerName: config.AIBackendCodex})
	if err == nil || m != nil {
		t.Fatalf("non-Claude secretary should be unavailable: manager=%v error=%v", m, err)
	}
}

func TestClaudeSecretaryUsesFullFlockAgentAndResumesBusinessChat(t *testing.T) {
	base := t.TempDir()
	template := filepath.Join(base, "template.md")
	if err := os.WriteFile(template, []byte("# Flock agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(base, "agents")
	if err := os.MkdirAll(agents, 0o750); err != nil {
		t.Fatal(err)
	}
	ws := &workspace.Renderer{BaseDir: base, TemplatePath: template, AgentsDir: agents}
	sessions, err := session.Open(filepath.Join(base, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &secretaryFakeRunner{events: []agent.Event{
		{Type: agent.SystemInit, SessionID: "business-session"},
		{Type: agent.ToolUse, Tool: "mcp__duckbug__search"},
		{Type: agent.ToolResult, Tool: "mcp__duckbug__search"},
		{Type: agent.Result, Result: &agent.RunResult{Text: "Full Flock answer", SessionID: "business-session"}},
	}}
	m, err := newSecretaryManager(config.Config{
		SecretaryMode: config.SecretaryModeAuto, ApprovedDirectory: base, AllowedUsers: []int64{10},
	}, secretaryRuntime{runner: runner, opts: agent.Options{
		Model: "selected-model", MCPConfig: filepath.Join(base, "mcp.json"),
		MaxTurns: 40, Effort: "ultracode", Env: []string{"FLOCK_TEST=1"},
	}, providerName: config.AIBackendClaude, workspace: ws, sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	api := &secretaryFakeAPI{connection: models.BusinessConnection{
		ID: "conn", User: models.User{ID: 10}, UserChatID: 100, IsEnabled: true,
		Rights: &models.BusinessBotRights{CanReply: true},
	}}
	msg := incomingSecretaryMessage()
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
	if len(api.sends) != 1 || api.sends[0].Text != "Full Flock answer" {
		t.Fatalf("business reply = %+v", api.sends)
	}
	if runner.calls != 1 || runner.prompts[0] != "Hello" {
		t.Fatalf("runner calls/prompts = %d/%q", runner.calls, runner.prompts)
	}
	first := runner.options[0]
	wantWorkdir := filepath.Join(base, "chat_"+secretaryChatKey("conn", 200))
	if first.AnswerOnly || first.MCPConfig != filepath.Join(base, "mcp.json") ||
		first.MaxTurns != 40 || first.Effort != "ultracode" || first.Model != "selected-model" ||
		first.Workdir != wantWorkdir || first.SessionID != "" || len(first.Env) != 1 {
		t.Fatalf("secretary did not inherit full agent options: %+v", first)
	}
	if _, err := os.Stat(filepath.Join(wantWorkdir, "CLAUDE.md")); err != nil {
		t.Fatalf("Flock workspace missing: %v", err)
	}
	msg.ID++
	msg.Text = "Follow up"
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
	if runner.calls != 2 || runner.prompts[1] != "Follow up" || runner.options[1].SessionID != "business-session" {
		t.Fatalf("business chat did not resume: calls=%d prompts=%q session=%q",
			runner.calls, runner.prompts, runner.options[1].SessionID)
	}
	msg.Chat.ID = 201
	msg.ID = 5
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
	if runner.calls != 3 || runner.options[2].SessionID != "" || runner.options[2].Workdir == first.Workdir {
		t.Fatalf("different business chat reused first session/workspace: %+v", runner.options[2])
	}
}

func TestSecretaryLongAnswerIsDeliveredWithoutTruncation(t *testing.T) {
	const connectionID = "conn"
	m, _, api := testSecretary(t, config.SecretaryModeApproval)
	msg := incomingSecretaryMessage()
	reply := strings.Repeat("Detailed answer. ", 650)
	m.queueApproval(context.Background(), api, &api.connection, msg, msg.Text, reply)
	if len(api.sends) < 2 {
		t.Fatalf("approval notices = %d, want multiple", len(api.sends))
	}
	for i, notice := range api.sends {
		if len([]rune(notice.Text)) > secretaryMaxNoticeRunes {
			t.Fatalf("notice %d exceeds Telegram limit", i)
		}
		if (notice.ReplyMarkup != nil) != (i == len(api.sends)-1) {
			t.Fatalf("approval buttons on notice %d", i)
		}
	}
	var pending secretaryPending
	for _, p := range m.state.Pending {
		pending = p
	}
	if pending.Reply != reply {
		t.Fatal("full answer was not stored for approval")
	}
	api.sends = nil
	if status := m.sendApproved(context.Background(), api, pending); status != "Sent." {
		t.Fatalf("sendApproved = %q", status)
	}
	parts := make([]string, 0, len(api.sends))
	for i, sent := range api.sends {
		if sent.BusinessConnectionID != connectionID || len([]rune(sent.Text)) > 4096 {
			t.Fatalf("business chunk %d invalid: %+v", i, sent)
		}
		parts = append(parts, sent.Text)
	}
	delivered := strings.Join(parts, " ")
	if strings.Join(strings.Fields(delivered), " ") != strings.Join(strings.Fields(reply), " ") {
		t.Fatalf("delivered %d runes, want %d", len([]rune(delivered)), len([]rune(reply)))
	}
}

func TestSecretarySkipsNonClaudeProviderWithoutStoppingBot(t *testing.T) {
	approved := t.TempDir()
	cfg := config.Config{
		AIBackend: config.AIBackendCodex, CodexAuthMode: config.CodexAuthSubscription,
		SecretaryMode: config.SecretaryModeApproval, ApprovedDirectory: approved,
	}
	if got := secretaryAllowedUpdates(cfg, agent.Options{}, config.AIBackendCodex); slices.Contains(
		got, models.AllowedUpdateBusinessMessage,
	) {
		t.Fatal("Codex subscription still requested business updates")
	}
	cleanup, err := wireSecretary(cfg, nil, slog.Default(), secretaryRuntime{
		runner: &secretaryFakeRunner{}, providerName: config.AIBackendCodex,
	})
	if err != nil || cleanup == nil {
		t.Fatalf("Codex subscription disabled the entire bot: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(approved, "secretary-state.json")); !os.IsNotExist(err) {
		t.Fatalf("Codex subscription initialized secretary state: %v", err)
	}
	cfg.CodexAuthMode = config.CodexAuthBilling
	if got := secretaryAllowedUpdates(cfg, agent.Options{}, config.AIBackendCodex); slices.Contains(
		got, models.AllowedUpdateBusinessMessage,
	) {
		t.Fatal("Codex billing requested business updates despite Claude-only support")
	}
	cfg.AIBackend = "codex-cli"
	cfg.CodexAuthMode = config.CodexAuthSubscription
	if got := secretaryAllowedUpdates(cfg, agent.Options{}, config.AIBackendCodex); slices.Contains(
		got, models.AllowedUpdateBusinessMessage,
	) {
		t.Fatal("Codex alias bypassed secretary availability check")
	}
}

func TestSecretaryRejectsFailedResult(t *testing.T) {
	m, runner, _ := testSecretary(t, config.SecretaryModeAuto)
	runner.events = []agent.Event{{Type: agent.Result, Result: &agent.RunResult{Text: "failed", IsError: true}}}
	if reply, err := m.draft(context.Background(), "chat", 10, "hello"); err == nil || reply != "" {
		t.Fatalf("failed result produced reply %q, error %v", reply, err)
	}
}

func TestSecretaryAutoRechecksRevokedConnectionAfterDraft(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	runner.onRun = func() {
		api.connection.IsEnabled = false
	}
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: incomingSecretaryMessage()})
	if runner.calls != 1 || len(api.sends) != 0 {
		t.Fatalf("revoked connection: drafts=%d sends=%d", runner.calls, len(api.sends))
	}
}

func TestSecretaryAutoRejectsRevocationDuringFinalLookup(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	api.onGet = func() {
		if api.gets != 3 {
			return
		}
		updated := api.connection
		updated.IsEnabled = false
		m.handleUpdate(context.Background(), api, &models.Update{BusinessConnection: &updated})
	}
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: incomingSecretaryMessage()})
	if runner.calls != 1 || len(api.sends) != 0 {
		t.Fatalf("in-flight revocation: drafts=%d sends=%d", runner.calls, len(api.sends))
	}
}

func TestSecretarySkipsUnsupportedMessagesWithoutConnectionLookup(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	msg := incomingSecretaryMessage()
	msg.Text = ""
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
	msg.Text = "Hello"
	msg.From.IsBot = true
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
	if api.gets != 0 || runner.calls != 0 {
		t.Fatalf("unsupported messages: connection lookups=%d drafts=%d", api.gets, runner.calls)
	}
}

func TestSecretaryConnectionUpdateWinsInFlightLookup(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeAuto)
	api.onGet = func() {
		updated := api.connection
		updated.Rights = &models.BusinessBotRights{CanReply: false}
		m.handleUpdate(context.Background(), api, &models.Update{BusinessConnection: &updated})
	}
	connection, err := m.connection(context.Background(), api, "conn")
	if err != nil || connection.Rights == nil || connection.Rights.CanReply {
		t.Fatalf("stale lookup restored revoked rights: connection=%+v error=%v", connection, err)
	}
}

func TestSecretaryUnseenEditDoesNotWriteState(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeApproval)
	m.handleUpdate(context.Background(), api, &models.Update{EditedBusinessMessage: incomingSecretaryMessage()})
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Fatalf("unseen edit wrote state: %v", err)
	}
}

func TestSecretaryEditBeforeMessagePreventsAutoReply(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	m.handleUpdate(context.Background(), api, &models.Update{EditedBusinessMessage: incomingSecretaryMessage()})
	m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: incomingSecretaryMessage()})
	if runner.calls != 0 || len(api.sends) != 0 {
		t.Fatal("reordered edit allowed an auto reply to stale message text")
	}
}

func TestSecretaryUntrustedStateIsBounded(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeAuto)
	now := time.Now().Unix()
	for i := range secretaryMaxInvalidKeys {
		m.state.Invalid[fmt.Sprintf("old:%d", i)] = now - 1
	}
	m.invalidate("conn", 200, []int{5})
	if len(m.state.Invalid) != secretaryMaxInvalidKeys {
		t.Fatalf("invalid keys=%d, want cap %d", len(m.state.Invalid), secretaryMaxInvalidKeys)
	}
	if _, ok := m.state.Invalid[secretaryKey("conn", 200, 5)]; !ok {
		t.Fatal("new cancellation was evicted")
	}
	for i := range secretaryMaxConnections {
		c := api.connection
		c.ID = fmt.Sprintf("conn-%d", i)
		m.cacheConnection(&c)
	}
	m.cacheConnection(&api.connection)
	if len(m.connections) != secretaryMaxConnections {
		t.Fatalf("cached connections=%d, want cap %d", len(m.connections), secretaryMaxConnections)
	}
	if _, ok := m.connections[api.connection.ID]; !ok {
		t.Fatal("new business connection was evicted")
	}
}

func TestSecretaryAutoOnlyRepliesToAllowedInboundMessageOnce(t *testing.T) {
	m, runner, api := testSecretary(t, "auto")
	ctx := context.Background()
	msg := incomingSecretaryMessage()
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: msg})
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: msg})
	if api.gets != 3 {
		t.Fatalf("business connection lookups=%d, want initial, pre-run, and pre-send checks", api.gets)
	}
	if runner.calls != 1 || len(api.sends) != 1 {
		t.Fatalf("calls=%d sends=%d, want one each", runner.calls, len(api.sends))
	}
	if api.sends[0].BusinessConnectionID != "conn" || api.sends[0].ChatID != int64(200) {
		t.Fatalf("wrong business send: %+v", api.sends[0])
	}
	ownerMessage := *msg
	ownerMessage.ID = 6
	ownerMessage.From = &models.User{ID: 10}
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: &ownerMessage})
	api.connection.Rights.CanReply = false
	m.handleUpdate(ctx, api, &models.Update{BusinessConnection: &api.connection})
	noRights := *msg
	noRights.ID = 7
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: &noRights})
	if runner.calls != 1 {
		t.Fatalf("owner/no-rights message generated reply: %d", runner.calls)
	}
}

func TestSecretaryApprovalRequiresOwnerAndIsSingleUse(t *testing.T) {
	m, _, api := testSecretary(t, "approval")
	ctx := context.Background()
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: incomingSecretaryMessage()})
	if len(api.sends) != 1 || api.sends[0].BusinessConnectionID != "" {
		t.Fatalf("approval must only notify owner: %+v", api.sends)
	}
	var token string
	for key := range m.state.Pending {
		token = key
	}
	if token == "" {
		t.Fatal("no pending approval")
	}
	callback := &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: 99}, Data: secretaryCallbackPrefix + "send:" + token,
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 1, Chat: models.Chat{ID: 100}, Text: "notice"}},
	}}
	m.handleCallback(ctx, api, callback)
	if len(api.sends) != 1 {
		t.Fatal("unauthorized callback sent a reply")
	}
	callback.CallbackQuery.From.ID = 10
	m.handleCallback(ctx, api, callback)
	m.handleCallback(ctx, api, callback)
	if len(api.sends) != 2 || api.sends[1].BusinessConnectionID != "conn" {
		t.Fatalf("approved reply count or routing wrong: %+v", api.sends)
	}
}

func TestSecretaryEditInvalidatesPendingDraft(t *testing.T) {
	m, _, api := testSecretary(t, "approval")
	ctx := context.Background()
	msg := incomingSecretaryMessage()
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: msg})
	m.handleUpdate(ctx, api, &models.Update{EditedBusinessMessage: msg})
	if len(m.state.Pending) != 0 {
		t.Fatal("edited message retained stale draft")
	}
}

func TestSecretaryApprovalDoesNotRestoreConsumedOrInvalidatedDraft(t *testing.T) {
	for _, action := range []string{"send", "edit"} {
		t.Run(action, func(t *testing.T) {
			m, _, api := testSecretary(t, "approval")
			ctx := context.Background()
			msg := incomingSecretaryMessage()
			api.onSend = func(p *bot.SendMessageParams) {
				if p.BusinessConnectionID != "" {
					return
				}
				if action == "edit" {
					m.invalidate("conn", 200, []int{5})
					return
				}
				for token := range m.state.Pending {
					m.handleCallback(ctx, api, &models.Update{CallbackQuery: &models.CallbackQuery{
						ID: "cb", From: models.User{ID: 10}, Data: secretaryCallbackPrefix + "send:" + token,
						Message: models.MaybeInaccessibleMessage{Message: &models.Message{
							ID: 1, Chat: models.Chat{ID: 100}, Text: "notice",
						}},
					}})
				}
			}
			m.handleUpdate(ctx, api, &models.Update{BusinessMessage: msg})
			if len(m.state.Pending) != 0 {
				t.Fatal("consumed or invalidated draft was restored")
			}
		})
	}
}

type secretaryBlockingRunner struct {
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

func (r *secretaryBlockingRunner) Run(ctx context.Context, _ string, _ agent.Options) (<-chan agent.Event, error) {
	out := make(chan agent.Event, 1)
	close(r.started)
	go func() {
		select {
		case <-r.release:
			out <- agent.Event{Type: agent.Result, Result: &agent.RunResult{Text: "Draft reply"}}
		case <-ctx.Done():
			if r.canceled != nil {
				close(r.canceled)
			}
		}
		close(out)
	}()
	return out, nil
}

func TestSecretaryEditWhileDraftingPreventsAutoSend(t *testing.T) {
	m, _, api := testSecretary(t, "auto")
	runner := &secretaryBlockingRunner{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	m.runner = runner
	msg := incomingSecretaryMessage()
	done := make(chan struct{})
	go func() {
		m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
		close(done)
	}()
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("draft did not start")
	}
	m.handleUpdate(context.Background(), api, &models.Update{EditedBusinessMessage: msg})
	select {
	case <-runner.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("edit did not cancel the active agent")
	}
	<-done
	if len(api.sends) != 0 {
		t.Fatal("auto replied to an edited message")
	}
}

func TestSecretaryRevocationCancelsActiveAgent(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeAuto)
	runner := &secretaryBlockingRunner{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	m.runner = runner
	msg := incomingSecretaryMessage()
	done := make(chan struct{})
	go func() {
		m.handleUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
		close(done)
	}()
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not start")
	}
	revoked := api.connection
	revoked.IsEnabled = false
	m.handleUpdate(context.Background(), api, &models.Update{BusinessConnection: &revoked})
	select {
	case <-runner.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not cancel the active agent")
	}
	<-done
	if len(api.sends) != 0 {
		t.Fatal("revoked agent replied")
	}
}

func TestSecretaryExpiredApprovalCannotSend(t *testing.T) {
	m, _, api := testSecretary(t, "approval")
	m.state.Pending["expired"] = secretaryPending{
		OwnerID: 10, OwnerChatID: 100, ConnectionID: "conn", ChatID: 200,
		MessageID: 5, NoticeID: 1, Reply: "Old draft",
		CreatedAt: time.Now().Add(-secretaryApprovalLifetime - time.Minute).Unix(),
	}
	m.handleCallback(context.Background(), api, &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: 10}, Data: secretaryCallbackPrefix + "send:expired",
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{
			ID: 1, Chat: models.Chat{ID: 100}, Text: "notice",
		}},
	}})
	if len(api.sends) != 0 {
		t.Fatal("expired draft was sent")
	}
}
