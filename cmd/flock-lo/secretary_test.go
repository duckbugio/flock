package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/core/session"
	"github.com/duckbugio/flock/core/workspace"
	"github.com/duckbugio/flock/internal/config"
)

const (
	secretaryTestConnection = "15eac687-9383-4da1-9644-826693923e44" //nolint:gosec // Public UUID fixture, not a credential.
	secretaryTestBot        = 1000000000000001
	secretaryTestAvailable  = "available"
)

type secretaryFakeAPI struct {
	connection     lo.SecretaryConnection
	actions        []lo.SecretaryAction
	drafts         int
	sends          int
	err            error
	onLookup       func()
	onNotice       func()
	onSend         func()
	noticeContexts []string
	noticeStatuses []string
	closeErr       error
}

func (api *secretaryFakeAPI) GetBusinessConnection(_ context.Context, _ string) (lo.SecretaryConnection, error) {
	if api.onLookup != nil {
		api.onLookup()
	}
	return api.connection, nil
}

func (api *secretaryFakeAPI) SendSecretaryReviewNotice(_ context.Context, _, _, _ int64, incoming, _, _ string) (int64, error) {
	api.drafts++
	api.noticeContexts = append(api.noticeContexts, incoming)
	if api.onNotice != nil {
		api.onNotice()
	}
	return 99, api.err
}

func (api *secretaryFakeAPI) CloseSecretaryReviewNotice(_ context.Context, _, _, _ int64, status string) error {
	api.noticeStatuses = append(api.noticeStatuses, status)
	return api.closeErr
}

func (api *secretaryFakeAPI) SendSecretaryText(_ context.Context, action lo.SecretaryAction, _ int64) error {
	api.sends++
	if api.onSend != nil {
		api.onSend()
	}
	api.actions = append(api.actions, action)
	return api.err
}

type secretaryFakeRunner struct {
	calls  int
	opts   []agent.Options
	prompt string
	onRun  func(context.Context)
	result *agent.RunResult
}

func (runner *secretaryFakeRunner) Run(ctx context.Context, prompt string, opts agent.Options) (<-chan agent.Event, error) {
	runner.calls++
	runner.opts = append(runner.opts, opts)
	runner.prompt = prompt
	if runner.onRun != nil {
		runner.onRun(ctx)
	}
	result := runner.result
	if result == nil {
		result = &agent.RunResult{Text: "Agent reply", SessionID: "secretary-session"}
	}
	ch := make(chan agent.Event, 3)
	ch <- agent.Event{Type: agent.Text, Text: "private progress"}
	ch <- agent.Event{Type: agent.ToolUse}
	ch <- agent.Event{Type: agent.Result, Result: result}
	close(ch)
	return ch, nil
}

type secretaryFakeVoice struct {
	calls  int
	fileID string
}

func (voice *secretaryFakeVoice) TranscribeRecording(_ context.Context, fileID, _ string) (string, error) {
	voice.calls++
	voice.fileID = fileID
	return "voice request", nil
}

func secretaryFixture(t *testing.T, mode string) (*secretaryManager, *secretaryFakeAPI, *secretaryFakeRunner) {
	t.Helper()
	root := t.TempDir()
	sessions, err := session.Open(filepath.Join(root, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := dispatch.New(1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		if err := dispatcher.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	cfg := config.Config{
		SecretaryMode: mode, SecretaryWorkspaceDir: filepath.Join(root, "business"),
		ApprovedDirectory:       filepath.Join(root, "ordinary"),
		LOSecretaryTemplatePath: filepath.Join("..", "..", "core", "CLAUDE.secretary-lo.md.tmpl"),
		TeamAgentsDir:           filepath.Join("..", "..", "core", "agents"), LOAllowedUsers: []int64{1},
	}
	runner := &secretaryFakeRunner{}
	api := &secretaryFakeAPI{connection: lo.SecretaryConnection{
		ID: secretaryTestConnection, User: lo.User{ID: 1},
		Enabled: true, SchemaVersion: 1, PolicyVersion: 1, Date: 1, Rights: []string{"receive_messages", "send_messages"},
	}}
	manager, err := newSecretaryManager(cfg, api, secretaryTestBot, secretaryRuntime{
		runner: runner, providerName: config.AIBackendClaude, opts: agent.Options{Model: "selected-model", MaxTurns: 7},
		workspace: &workspace.Renderer{BaseDir: cfg.ApprovedDirectory, AgentsDir: cfg.TeamAgentsDir},
		sessions:  sessions, dispatcher: dispatcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, api, runner
}

func secretaryMessage() lo.SecretaryMessage {
	var msg lo.SecretaryMessage
	// Native wire fixture: int64 IDs can be strings, and voice data is nested under its kind.
	err := json.Unmarshal([]byte(`{"message_id":12,"from":{"id":77,"is_bot":false},"chat":{"id":77,"type":`+
		`"private"},"date":1,"text":"hello","business_connection_id":"`+secretaryTestConnection+
		`","lo_event_id":"incoming","lo_context":{"conversation_id":19,"chat_id":`+
		`77,"policy_version":1,"source_message_id":12,"source_revision":1}}`), &msg)
	if err != nil {
		panic(err)
	}
	msg.Date = time.Now().Unix()
	return msg
}

func admitSecretary(t *testing.T, manager *secretaryManager, msg lo.SecretaryMessage) string {
	t.Helper()
	if err := manager.Handle(t.Context(), lo.Update{ID: int64(msg.ID), Delegated: true, BusinessMessage: &msg}); err != nil {
		t.Fatal(err)
	}
	return secretaryJobKey(manager.botID, msg)
}

func TestSecretaryFullAgentBotReviewAndAuto(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.SecretaryModeApproval, config.SecretaryModeAuto} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			manager, api, runner := secretaryFixture(t, mode)
			msg := secretaryMessage()
			key := admitSecretary(t, manager, msg)
			manager.process(t.Context(), key)
			if mode == config.SecretaryModeApproval {
				if manager.state.Jobs[key].Status != secretaryAwaiting || api.sends != 0 {
					t.Fatal("reply sent without owner approval")
				}
				toast := manager.ReviewCallback(t.Context(), secretaryReviewQuery(manager, key, "send"))
				if !strings.Contains(toast, "approved") {
					t.Fatal(toast)
				}
				manager.process(t.Context(), key)
			}
			if runner.calls != 1 || len(api.actions) != 1 || api.actions[0].Context != msg.Context ||
				api.actions[0].RequestID != "flock:"+key || api.actions[0].Text != "Agent reply" {
				t.Fatal("full agent or native delivery missing")
			}
			if api.drafts != boolCount(mode == config.SecretaryModeApproval) ||
				api.sends != 1 {
				t.Fatal("wrong delivery mode")
			}
			opts := runner.opts[0]
			if opts.AnswerOnly || opts.MaxTurns != 7 || opts.Model != "selected-model" ||
				!inside(manager.runtime.workspace.BaseDir, opts.Workdir) {
				t.Fatalf("agent options: %+v", opts)
			}
			data, err := os.ReadFile(filepath.Join(opts.Workdir, "CLAUDE.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "LO secretary") || strings.Contains(string(data), "Telegram Business") {
				t.Fatal("wrong role")
			}
			sid, ok := manager.runtime.sessions.Get(secretarySessionKey(manager.state.Jobs[key]))
			if !ok || sid != "secretary-session" {
				t.Fatal("session not persisted")
			}
			if err := manager.Handle(t.Context(), lo.Update{ID: 2, BusinessMessage: &msg}); err != nil {
				t.Fatal(err)
			}
			manager.process(t.Context(), key)
			if runner.calls != 1 || len(api.actions) != 1 {
				t.Fatal("redelivery reran tools or sent twice")
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestSecretaryUncertainDeliverySurvivesRestartWithoutRerunningTools(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	api.err = errors.New("uncertain timeout")
	key := admitSecretary(t, manager, secretaryMessage())
	manager.process(t.Context(), key)
	if manager.state.Jobs[key].Status != secretaryPrepared {
		t.Fatal("uncertain write was discarded")
	}
	first := api.actions[0]
	reloaded, err := newSecretaryManager(manager.cfg, api, manager.botID, manager.runtime)
	if err != nil {
		t.Fatal(err)
	}
	api.err = nil
	reloaded.process(t.Context(), key)
	if runner.calls != 1 || len(api.actions) != 2 || api.actions[1] != first || reloaded.state.Jobs[key].Status != secretaryDone {
		t.Fatal("retry changed request or repeated tools")
	}
	info, err := os.Stat(manager.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("private state permissions lost")
	}
}

func TestSecretaryInvalidationCancelsInflightAgent(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"revoke", "edit", "delete", "policy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
			msg := secretaryMessage()
			key := admitSecretary(t, manager, msg)
			runner.onRun = func(ctx context.Context) {
				update := lo.Update{}
				switch kind {
				case "revoke":
					conn := api.connection
					conn.Enabled = false
					update.BusinessConnection = &conn
				case "policy":
					conn := api.connection
					conn.PolicyVersion++
					update.BusinessConnection = &conn
				case "edit":
					edited := msg
					edited.Context.SourceRevision++
					update.EditedBusinessMessage = &edited
				case "delete":
					deletion := &lo.SecretaryDeletion{
						ConnectionID: msg.ConnectionID, Context: msg.Context,
						MessageIDs: []lo.SecretaryNumber{msg.ID},
					}
					update.DeletedBusinessMessages = deletion
				}
				if err := manager.Handle(ctx, update); err != nil {
					t.Error(err)
				}
				if ctx.Err() == nil {
					t.Error("agent context not cancelled")
				}
			}
			manager.process(t.Context(), key)
			if len(api.actions) != 0 || manager.state.Jobs[key].Status != secretaryCancelled {
				t.Fatal("invalidated reply delivered")
			}
		})
	}
}

func TestSecretaryOwnerRightsAndEchoGuards(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"owner", "unlisted", "receive-only", "send-only", "stale", "echo"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
			msg := secretaryMessage()
			switch kind {
			case "owner":
				msg.From.ID = 1
			case "unlisted":
				api.connection.User.ID = 2
			case "receive-only":
				api.connection.Rights = []string{"receive_messages"}
			case "send-only":
				api.connection.Rights = []string{"send_messages"}
			case "stale":
				api.connection.PolicyVersion++
			case "echo":
				msg.BotID = secretaryTestBot
			}
			key := admitSecretary(t, manager, msg)
			manager.process(t.Context(), key)
			if runner.calls != 0 || len(api.actions) != 0 {
				t.Fatal("unauthorized run admitted")
			}
		})
	}
}

func TestSecretaryNativeVoiceAndRevocationBeforeTranscription(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	voice := &secretaryFakeVoice{}
	manager.runtime.voice = voice
	msg := secretaryMessage()
	msg.Text = ""
	msg.MediaStatus = secretaryTestAvailable
	msg.Attachments = []lo.SecretaryAttachment{{
		Kind:  "voice",
		Voice: &lo.Attachment{FileID: "secretary-v1:delegated:abcdefghijklmnopqrstuv"},
	}}
	key := admitSecretary(t, manager, msg)
	manager.process(t.Context(), key)
	if voice.calls != 1 || !strings.Contains(runner.prompt, "voice request") || api.drafts != 1 {
		t.Fatal("native voice not transcribed")
	}
	msg.ID++
	msg.Context.SourceMessageID = msg.ID
	msg.EventID = "second"
	key = admitSecretary(t, manager, msg)
	api.connection.Enabled = false
	manager.process(t.Context(), key)
	if voice.calls != 1 || runner.calls != 1 {
		t.Fatal("revoked voice reached paid provider")
	}
}

func TestSecretaryPersistedInvalidationBeforeOriginalAndInterruptedRun(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	msg := secretaryMessage()
	if err := manager.Handle(t.Context(), lo.Update{EditedBusinessMessage: &msg}); err != nil {
		t.Fatal(err)
	}
	reload, err := newSecretaryManager(manager.cfg, api, manager.botID, manager.runtime)
	if err != nil {
		t.Fatal(err)
	}
	admitSecretary(t, reload, msg)
	if len(reload.state.Jobs) != 0 {
		t.Fatal("old source admitted after persisted edit")
	}
	msg.ID++
	msg.Context.SourceMessageID = msg.ID
	key := admitSecretary(t, reload, msg)
	job := reload.state.Jobs[key]
	job.Status = secretaryRunning
	if !reload.store(key, job) {
		t.Fatal("could not save running job")
	}
	reload, err = newSecretaryManager(manager.cfg, api, manager.botID, manager.runtime)
	if err != nil {
		t.Fatal(err)
	}
	reload.process(t.Context(), key)
	if runner.calls != 0 || reload.state.Jobs[key].Status != secretaryCancelled {
		t.Fatal("interrupted tools replayed")
	}
}

func TestSecretaryPersistenceFailureStopsBeforeGeneration(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	manager.path = filepath.Join(t.TempDir(), "missing", "state.json")
	msg := secretaryMessage()
	if err := manager.Handle(t.Context(), lo.Update{BusinessMessage: &msg}); err == nil {
		t.Fatal("persistence failure hidden")
	}
	manager.process(t.Context(), secretaryJobKey(manager.botID, msg))
	if runner.calls != 0 || len(api.actions) != 0 || manager.err() == nil {
		t.Fatal("continued after state write failure")
	}
}

func TestSecretarySharedDispatcherDoesNotBlockRevocation(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	entered := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	runner.onRun = func(ctx context.Context) { once.Do(func() { close(entered) }); <-ctx.Done(); close(finished) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go manager.Pump(ctx)
	msg := secretaryMessage()
	admitSecretary(t, manager, msg)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("agent not started")
	}
	conn := api.connection
	conn.Enabled = false
	if err := manager.Handle(t.Context(), lo.Update{BusinessConnection: &conn}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("poll-side revocation blocked")
	}
	// Drain the worker before inspecting the fake's nonconcurrent action counter.
	if err := manager.runtime.dispatcher.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(api.actions) != 0 {
		t.Fatal("revoked dispatcher run sent")
	}
}

func TestSecretaryRevocationWinsAgainstInflightConsentLookup(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	msg := secretaryMessage()
	key := admitSecretary(t, manager, msg)
	api.onLookup = func() {
		conn := api.connection
		conn.Enabled = false
		if err := manager.Handle(t.Context(), lo.Update{BusinessConnection: &conn}); err != nil {
			t.Error(err)
		}
	}
	manager.process(t.Context(), key)
	if runner.calls != 0 || len(api.actions) != 0 {
		t.Fatal("stale lookup overwrote revocation")
	}
}

func TestSecretaryRestartRejectsWrongBotAndCancelsOldMode(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	api.err = errors.New("uncertain delivery")
	key := admitSecretary(t, manager, secretaryMessage())
	manager.process(t.Context(), key)
	if _, err := newSecretaryManager(manager.cfg, api, manager.botID+1, manager.runtime); err == nil {
		t.Fatal("state from another bot accepted")
	}
	cfg := manager.cfg
	cfg.SecretaryMode = config.SecretaryModeApproval
	reloaded, err := newSecretaryManager(cfg, api, manager.botID, manager.runtime)
	if err != nil {
		t.Fatal(err)
	}
	api.err = nil
	reloaded.process(t.Context(), key)
	if runner.calls != 1 || len(api.actions) != 1 || reloaded.state.Jobs[key].Status != secretaryUnknown {
		t.Fatal("old automatic mode replayed under owner-review configuration")
	}
}

func TestSecretarySessionIsolationAndTerminalFailure(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	msg := secretaryMessage()
	key := admitSecretary(t, manager, msg)
	manager.process(t.Context(), key)
	firstSession := secretarySessionKey(manager.state.Jobs[key])
	msg.ID++
	msg.Context.SourceMessageID = msg.ID
	msg.EventID = "next"
	key = admitSecretary(t, manager, msg)
	manager.process(t.Context(), key)
	if runner.opts[1].SessionID != "secretary-session" {
		t.Fatal("same conversation did not resume")
	}
	api.connection.PolicyVersion++
	msg.Context.PolicyVersion++
	msg.ID++
	msg.Context.SourceMessageID = msg.ID
	key = admitSecretary(t, manager, msg)
	manager.process(t.Context(), key)
	if runner.opts[2].SessionID != "" || firstSession == secretarySessionKey(manager.state.Jobs[key]) {
		t.Fatal("policy generation reused previous session")
	}
	runner.result = &agent.RunResult{Text: "partial answer", IsError: true}
	msg.ID++
	msg.Context.SourceMessageID = msg.ID
	key = admitSecretary(t, manager, msg)
	manager.process(t.Context(), key)
	if api.drafts != 3 || api.sends != 0 {
		t.Fatal("failed terminal result delivered")
	}
	if _, ok := manager.runtime.sessions.Get(secretarySessionKey(manager.state.Jobs[key])); ok {
		t.Fatal("failed resumed session retained")
	}
}

func TestSecretaryCostAndRateLimitsBeforeVoiceAndAgent(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"cost", "rate"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			manager, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
			voice := &secretaryFakeVoice{}
			manager.runtime.voice = voice
			if kind == "cost" {
				costs, err := cost.Open(filepath.Join(t.TempDir(), "costs.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := costs.Add(1, 2); err != nil {
					t.Fatal(err)
				}
				manager.runtime.costs = costs
				manager.cfg.ClaudeMaxCostPerUser = 1
			} else {
				manager.limit = ratelimit.New(1, time.Minute)
				manager.limit.Allow(1, time.Now())
			}
			msg := secretaryMessage()
			msg.Text = ""
			msg.MediaStatus = secretaryTestAvailable
			msg.Attachments = []lo.SecretaryAttachment{{
				Kind:  "voice",
				Voice: &lo.Attachment{FileID: "secretary-v1:payload:abcdefghijklmnopqrstuv"},
			}}
			key := admitSecretary(t, manager, msg)
			manager.process(t.Context(), key)
			if voice.calls != 0 || runner.calls != 0 || len(api.actions) != 0 {
				t.Fatal("guard was bypassed")
			}
		})
	}
}

func TestSecretaryCannotDeliverBeforePreparedStateIsPersisted(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	key := admitSecretary(t, manager, secretaryMessage())
	runner.onRun = func(_ context.Context) { manager.path = filepath.Join(t.TempDir(), "missing", "state.json") }
	manager.process(t.Context(), key)
	if runner.calls != 1 || len(api.actions) != 0 || manager.err() == nil {
		t.Fatal("nonpersisted prepared reply delivered")
	}
}
