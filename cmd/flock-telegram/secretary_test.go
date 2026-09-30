package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
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

type secretaryFakeVoice struct {
	fileIDs []string
	text    string
	err     error
}

func (v *secretaryFakeVoice) Transcribe(_ context.Context, fileID string) (string, error) {
	v.fileIDs = append(v.fileIDs, fileID)
	return v.text, v.err
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
	failSendAt int
	sendCalls  int
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
	f.sendCalls++
	if f.sendCalls == f.failSendAt {
		return nil, errors.New("simulated send failure")
	}
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
		limit:     ratelimit.New(30, time.Minute), dispatcher: dispatch.New(4),
		state:  secretaryState{Seen: map[string]int64{}, Invalid: map[string]int64{}, Pending: map[string]secretaryPending{}},
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

func TestSecretaryVoiceUsesNormalTranscription(t *testing.T) {
	for _, mode := range []string{config.SecretaryModeAuto, config.SecretaryModeApproval} {
		t.Run(mode, func(t *testing.T) {
			m, runner, api := testSecretary(t, mode)
			voice := &secretaryFakeVoice{text: "Please review the changes"}
			m.voice = voice
			msg := incomingSecretaryMessage()
			msg.Text = ""
			msg.Voice = &models.Voice{FileID: "voice-file"}
			update := &models.Update{BusinessMessage: msg}
			m.handleUpdate(t.Context(), api, update)
			m.handleUpdate(t.Context(), api, update)
			if len(voice.fileIDs) != 1 || voice.fileIDs[0] != "voice-file" || runner.calls != 1 ||
				!strings.Contains(runner.prompts[0], strconv.Quote(voice.text)) {
				t.Fatalf("voice was not transcribed once into the Flock run: voice=%v runs=%d prompts=%v",
					voice.fileIDs, runner.calls, runner.prompts)
			}
			if mode == config.SecretaryModeAuto {
				if len(api.sends) != 1 || api.sends[0].BusinessConnectionID != api.connection.ID {
					t.Fatalf("automatic Business reply = %+v", api.sends)
				}
			} else if len(m.state.Pending) != 1 || len(api.sends) != 1 ||
				!strings.Contains(api.sends[0].Text, voice.text) {
				t.Fatalf("approval draft did not include transcript: pending=%v sends=%+v", m.state.Pending, api.sends)
			}
		})
	}
}

func TestSecretaryVoiceRequiresConfiguredTranscriberAndBusinessRights(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	msg := incomingSecretaryMessage()
	msg.Text = ""
	msg.Voice = &models.Voice{FileID: "voice-file"}
	update := &models.Update{BusinessMessage: msg}
	m.handleUpdate(t.Context(), api, update)
	if api.gets != 0 || runner.calls != 0 {
		t.Fatalf("disabled voice made an API or model call: gets=%d runs=%d", api.gets, runner.calls)
	}
	voice := &secretaryFakeVoice{text: "transcript"}
	m.voice = voice
	api.connection.Rights.CanReply = false
	m.handleUpdate(t.Context(), api, update)
	if len(voice.fileIDs) != 0 || runner.calls != 0 {
		t.Fatalf("unauthorized voice was transcribed or submitted: voice=%v runs=%d", voice.fileIDs, runner.calls)
	}
}

func TestSecretaryVoiceFailureNotifiesOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		err  error
		want string
	}{
		{name: "provider error", err: errors.New("speech service failed"), want: "could not transcribe"},
		{name: "empty transcript", text: "  ", want: "could not make out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, runner, api := testSecretary(t, config.SecretaryModeAuto)
			m.voice = &secretaryFakeVoice{text: tc.text, err: tc.err}
			msg := incomingSecretaryMessage()
			msg.Text = ""
			msg.Voice = &models.Voice{FileID: "voice-file"}
			m.handleUpdate(t.Context(), api, &models.Update{BusinessMessage: msg})
			if runner.calls != 0 || len(api.sends) != 1 || api.sends[0].ChatID != int64(100) ||
				!strings.Contains(api.sends[0].Text, tc.want) {
				t.Fatalf("voice failure not reported to owner: runs=%d sends=%+v", runner.calls, api.sends)
			}
		})
	}
}

func TestSecretaryVoiceTranscriptionErrorAllowsRetry(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	voice := &secretaryFakeVoice{err: errors.New("temporary speech service failure")}
	m.voice = voice
	msg := incomingSecretaryMessage()
	msg.Text = ""
	msg.Voice = &models.Voice{FileID: "voice-file"}
	update := &models.Update{BusinessMessage: msg}
	m.handleUpdate(t.Context(), api, update)
	if _, seen := m.state.Seen[secretaryMessageKey(msg)]; seen {
		t.Fatal("temporary transcription failure kept the message claimed")
	}
	voice.err = nil
	voice.text = "Retry this request"
	m.handleUpdate(t.Context(), api, update)
	if len(voice.fileIDs) != 2 || runner.calls != 1 || len(api.sends) != 2 ||
		api.sends[1].BusinessConnectionID != api.connection.ID {
		t.Fatalf("redelivery did not reach the agent: voice=%v runs=%d sends=%+v",
			voice.fileIDs, runner.calls, api.sends)
	}
}

func TestSecretaryVoiceChecksCostCapBeforeTranscription(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	voice := &secretaryFakeVoice{text: "Should not transcribe"}
	m.voice = voice
	costs, err := cost.Open(filepath.Join(t.TempDir(), "costs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := costs.Add(10, 1); err != nil {
		t.Fatal(err)
	}
	m.costs = costs
	m.costCapUSD = 1
	msg := incomingSecretaryMessage()
	msg.Text = ""
	msg.Voice = &models.Voice{FileID: "voice-file"}
	m.handleUpdate(t.Context(), api, &models.Update{BusinessMessage: msg})
	if len(voice.fileIDs) != 0 || runner.calls != 0 || len(api.sends) != 1 ||
		api.sends[0].ChatID != int64(100) || !strings.Contains(api.sends[0].Text, "cost limit") {
		t.Fatalf("cost-capped voice was transcribed or not reported: voice=%v runs=%d sends=%+v",
			voice.fileIDs, runner.calls, api.sends)
	}
	if _, seen := m.state.Seen[secretaryMessageKey(msg)]; seen {
		t.Fatal("cost-capped voice kept the message claimed")
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

func TestPrepareSecretaryRuntimeUsesSeparateWorkspaceAndMCPConfig(t *testing.T) {
	base := t.TempDir()
	approved := filepath.Join(base, "ordinary")
	business := filepath.Join(base, "business")
	if err := os.MkdirAll(approved, 0o700); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(approved, ".flock-mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"example":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := prepareSecretaryRuntime(config.Config{
		ApprovedDirectory: approved, SecretaryWorkspaceDir: business,
	}, secretaryRuntime{workspace: &workspace.Renderer{BaseDir: approved}, opts: agent.Options{MCPConfig: mcp}})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.workspace.BaseDir != business || !runtime.workspace.FileDeliveryDisabled ||
		!runtime.workspace.FollowupsDisabled || runtime.opts.MCPConfig == mcp {
		t.Fatalf("business workspace not separated: %+v %+v", runtime.workspace, runtime.opts)
	}
	mcpCopy, err := os.ReadFile(runtime.opts.MCPConfig)
	if err != nil || string(mcpCopy) != `{"mcpServers":{"example":{}}}` {
		t.Fatalf("business MCP copy = %q, %v", mcpCopy, err)
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
	if m.timeout != secretaryRunTimeout {
		t.Fatalf("secretary run timeout = %v, want %v", m.timeout, secretaryRunTimeout)
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
	if runner.calls != 1 || !strings.Contains(runner.prompts[0], strconv.Quote("Hello")) ||
		!strings.Contains(runner.prompts[0], "untrusted") {
		t.Fatalf("runner calls/prompts = %d/%q", runner.calls, runner.prompts)
	}
	first := runner.options[0]
	wantWorkdir := filepath.Join(base, "chat_"+secretaryChatKey(10, 200))
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
	if runner.calls != 2 || !strings.Contains(runner.prompts[1], strconv.Quote("Follow up")) ||
		runner.options[1].SessionID != "business-session" {
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
		if (notice.ReplyMarkup != nil) != (i == 0) {
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

func TestSecretaryApprovalKeepsButtonsAfterLaterChunkFails(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeApproval)
	api.failSendAt = 2
	msg := incomingSecretaryMessage()
	reply := strings.Repeat("Detailed answer. ", 650)
	m.queueApproval(context.Background(), api, &api.connection, msg, msg.Text, reply)
	if len(api.sends) < 2 || api.sends[0].ReplyMarkup == nil {
		t.Fatalf("first preview must retain approval buttons: %+v", api.sends)
	}
	if !strings.Contains(api.sends[len(api.sends)-1].Text, "Draft preview was truncated") {
		t.Fatalf("owner did not receive truncated preview notice: %+v", api.sends)
	}
	if len(m.state.Pending) != 1 {
		t.Fatalf("approval draft was discarded after later chunk failure: %+v", m.state.Pending)
	}
	for _, pending := range m.state.Pending {
		if pending.NoticeID != 1 || pending.Reply != reply {
			t.Fatalf("approval draft lost buttons or full answer: %+v", pending)
		}
	}
}

func TestSecretarySkipsNonClaudeProviderWithoutStoppingBot(t *testing.T) {
	approved := t.TempDir()
	cfg := config.Config{
		AIBackend: config.AIBackendCodex, CodexAuthMode: config.CodexAuthSubscription,
		SecretaryMode: config.SecretaryModeApproval, ApprovedDirectory: approved,
	}
	if got := secretaryAllowedUpdates(cfg, config.AIBackendCodex); slices.Contains(
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
	if got := secretaryAllowedUpdates(cfg, config.AIBackendCodex); slices.Contains(
		got, models.AllowedUpdateBusinessMessage,
	) {
		t.Fatal("Codex billing requested business updates despite Claude-only support")
	}
	cfg.AIBackend = "codex-cli"
	cfg.CodexAuthMode = config.CodexAuthSubscription
	if got := secretaryAllowedUpdates(cfg, config.AIBackendCodex); slices.Contains(
		got, models.AllowedUpdateBusinessMessage,
	) {
		t.Fatal("Codex alias bypassed secretary availability check")
	}
}

func TestSecretaryRejectsFailedResult(t *testing.T) {
	m, runner, _ := testSecretary(t, config.SecretaryModeAuto)
	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	status := 529
	runner.events = []agent.Event{{Type: agent.Result, Result: &agent.RunResult{
		Text: "private conversation text", IsError: true, Subtype: "error_during_execution",
		TerminalReason: "api_error", APIErrorStatus: &status, NumTurns: 2, DurationMS: 500,
	}}}
	if reply, err := m.draft(context.Background(), "chat", 10, "hello"); !errors.Is(err, errSecretaryAIRunFailed) || reply != "" {
		t.Fatalf("failed result produced reply %q, error %v", reply, err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logOutput.Bytes()), &record); err != nil {
		t.Fatalf("decode structured failure log: %v", err)
	}
	if record["owner_id"] != float64(10) || record["chat_key"] != "chat" ||
		record["subtype"] != "error_during_execution" || record["terminal_reason"] != "api_error" ||
		record["api_status"] != float64(529) || record["turns"] != float64(2) ||
		record["duration_ms"] != float64(500) || record["text_len"] != float64(len("private conversation text")) ||
		strings.Contains(logOutput.String(), "private conversation text") {
		t.Fatalf("failure log lost structured diagnostics or exposed private text: %v", record)
	}
}

func TestSecretaryErrorCodePreservesNewReasons(t *testing.T) {
	if got := secretaryErrorCode("api.error: новое"); got != "api.error: новое" {
		t.Fatalf("new provider reason was discarded: %q", got)
	}
	if got := secretaryErrorCode(strings.Repeat("я", 70)); len([]rune(got)) != 64 {
		t.Fatalf("provider reason was not bounded by runes: %d", len([]rune(got)))
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
	if api.sends[0].BusinessConnectionID != msg.BusinessConnectionID || api.sends[0].ChatID != int64(200) {
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

func TestSecretaryBotHandlerRemainsResponsiveDuringBusinessRun(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeAuto)
	runner := &secretaryBlockingRunner{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	m.runner = runner
	msg := incomingSecretaryMessage()
	returned := make(chan struct{})
	go func() {
		m.handleBotUpdate(context.Background(), api, &models.Update{BusinessMessage: msg})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Business update blocked the Telegram handler")
	}
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("Business run did not start")
	}
	m.handleBotUpdate(context.Background(), api, &models.Update{EditedBusinessMessage: msg})
	select {
	case <-runner.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("edit did not cancel the Business run")
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.dispatcher.Shutdown(drainCtx); err != nil {
		t.Fatalf("cancelled Business run did not finish: %v", err)
	}
	if len(api.sends) != 0 {
		t.Fatal("edited Business message received an auto reply")
	}
}

func TestSecretaryBusinessRunSurvivesUpdateCancellationUntilDrain(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeAuto)
	runner := &secretaryBlockingRunner{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	m.runner = runner
	updateCtx, cancelUpdate := context.WithCancel(context.Background())
	m.handleBotUpdate(updateCtx, api, &models.Update{BusinessMessage: incomingSecretaryMessage()})
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("Business run did not start")
	}
	cancelUpdate()
	select {
	case <-runner.canceled:
		t.Fatal("update cancellation interrupted the Business run before drain")
	default:
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDrain()
	drained := make(chan error, 1)
	go func() { drained <- m.dispatcher.Shutdown(drainCtx) }()
	select {
	case err := <-drained:
		t.Fatalf("drain returned before active Business run finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(runner.release)
	if err := <-drained; err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(api.sends) != 1 || api.sends[0].BusinessConnectionID != incomingSecretaryMessage().BusinessConnectionID {
		t.Fatalf("Business reply after drain = %+v", api.sends)
	}
}

func TestSecretaryBusinessMessagesStayInDispatchOrder(t *testing.T) {
	m, runner, api := testSecretary(t, config.SecretaryModeAuto)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	replies := make(chan struct{}, 2)
	runner.onRun = func() {
		if runner.calls == 1 {
			close(firstStarted)
			<-releaseFirst
		}
	}
	api.onSend = func(p *bot.SendMessageParams) {
		if p.BusinessConnectionID != "" {
			replies <- struct{}{}
		}
	}
	first := incomingSecretaryMessage()
	second := *first
	second.ID++
	second.Text = "Second message"
	m.handleBotUpdate(context.Background(), api, &models.Update{BusinessMessage: first})
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first Business run did not start")
	}
	m.handleBotUpdate(context.Background(), api, &models.Update{BusinessMessage: &second})
	close(releaseFirst)
	for range 2 {
		select {
		case <-replies:
		case <-time.After(3 * time.Second):
			t.Fatal("queued Business message did not get a reply")
		}
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.dispatcher.Shutdown(drainCtx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if runner.calls != 2 || !strings.Contains(runner.prompts[0], strconv.Quote("Hello")) ||
		!strings.Contains(runner.prompts[1], strconv.Quote("Second message")) {
		t.Fatalf("Business prompts ran out of order: %q", runner.prompts)
	}
}

func TestSecretaryQueueOverflowNotifiesOwner(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeAuto)
	runner := &secretaryBlockingRunner{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	m.runner = runner
	first := incomingSecretaryMessage()
	m.handleBotUpdate(context.Background(), api, &models.Update{BusinessMessage: first})
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first Business run did not start")
	}
	// The dispatcher buffers 64 jobs per chat while the first run is active.
	for i := range 65 {
		msg := *first
		msg.ID = first.ID + i + 1
		m.handleBotUpdate(context.Background(), api, &models.Update{BusinessMessage: &msg})
	}
	if len(api.sends) != 1 || !strings.Contains(api.sends[0].Text, "Secretary is busy") {
		t.Fatalf("queue overflow notice = %+v", api.sends)
	}
	m.dispatcher.Close()
	select {
	case <-runner.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("first Business run was not cancelled on close")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.Lock()
		active := len(m.active)
		m.mu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled Business run did not finish before test cleanup")
		}
		time.Sleep(time.Millisecond)
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
