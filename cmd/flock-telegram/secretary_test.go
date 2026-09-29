package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/internal/config"
)

type secretaryFakeRunner struct{ calls int }

func (r *secretaryFakeRunner) Run(_ context.Context, _ string, _ agent.Options) (<-chan agent.Event, error) {
	r.calls++
	ch := make(chan agent.Event, 1)
	ch <- agent.Event{Type: agent.Result, Result: &agent.RunResult{Text: "Draft reply"}}
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
}

func (f *secretaryFakeAPI) GetBusinessConnection(
	_ context.Context, _ *bot.GetBusinessConnectionParams,
) (*models.BusinessConnection, error) {
	f.gets++
	c := f.connection
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
	m := &secretaryManager{
		mode: mode, runner: runner, path: filepath.Join(t.TempDir(), "secretary.json"),
		allow: func(id int64) bool { return id == 10 },
		limit: ratelimit.New(30, time.Minute), sem: make(chan struct{}, 4),
		state: secretaryState{Seen: map[string]int64{}, Invalid: map[string]int64{}, Pending: map[string]secretaryPending{}},
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

func TestSecretaryModelOverride(t *testing.T) {
	m, err := newSecretaryManager(config.Config{
		SecretaryMode: config.SecretaryModeAuto, SecretaryModel: "reply-model",
		OpenAICompatModel: "coding-model", ApprovedDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.opts.Model != "reply-model" {
		t.Fatalf("model=%q, want reply-model", m.opts.Model)
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

func TestSecretaryUnseenEditDoesNotWriteState(t *testing.T) {
	m, _, api := testSecretary(t, config.SecretaryModeApproval)
	m.handleUpdate(context.Background(), api, &models.Update{EditedBusinessMessage: incomingSecretaryMessage()})
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Fatalf("unseen edit wrote state: %v", err)
	}
}

func TestSecretaryAutoOnlyRepliesToAllowedInboundMessageOnce(t *testing.T) {
	m, runner, api := testSecretary(t, "auto")
	ctx := context.Background()
	msg := incomingSecretaryMessage()
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: msg})
	m.handleUpdate(ctx, api, &models.Update{BusinessMessage: msg})
	if api.gets != 1 {
		t.Fatalf("business connection lookups=%d, want one cached lookup", api.gets)
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
	started chan struct{}
	release chan struct{}
}

func (r *secretaryBlockingRunner) Run(_ context.Context, _ string, _ agent.Options) (<-chan agent.Event, error) {
	out := make(chan agent.Event, 1)
	close(r.started)
	go func() {
		<-r.release
		out <- agent.Event{Type: agent.Result, Result: &agent.RunResult{Text: "Draft reply"}}
		close(out)
	}()
	return out, nil
}

func TestSecretaryEditWhileDraftingPreventsAutoSend(t *testing.T) {
	m, _, api := testSecretary(t, "auto")
	runner := &secretaryBlockingRunner{started: make(chan struct{}), release: make(chan struct{})}
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
	close(runner.release)
	<-done
	if len(api.sends) != 0 {
		t.Fatal("auto replied to an edited message")
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
