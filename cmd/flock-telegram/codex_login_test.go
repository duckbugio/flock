package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/codexauth"
	"github.com/duckbugio/flock/internal/config"
)

// unauthorizedCodexAuth returns a manager for a Codex deploy that has never been
// signed in: subscription mode over an empty CODEX_HOME (no auth.json). It also
// returns that home so a test can "complete" the login by planting the file.
func unauthorizedCodexAuth(t *testing.T) (*codexauth.Manager, string) {
	t.Helper()
	home := t.TempDir()
	return codexauth.NewManager(codexauth.Config{
		Backend:     codexauth.BackendCodex,
		AuthMode:    codexauth.AuthSubscription,
		RequireAuth: true,
		Home:        home,
	}), home
}

// The allowed sender and the private chat every case in this file uses.
const (
	loginTestUserID int64 = 42
	loginTestChatID int64 = 200
)

// textMessage builds a plain private-chat text message from the allowed sender.
func textMessage(text string) *models.Message {
	return &models.Message{
		ID:   1,
		Text: text,
		From: &models.User{ID: loginTestUserID},
		Chat: models.Chat{ID: loginTestChatID, Type: models.ChatTypePrivate},
	}
}

// quietBot builds a bot that answers every API call locally, so a handler that
// posts a reply is a no-op with no network.
func quietBot(t *testing.T) *bot.Bot {
	t.Helper()
	b, err := bot.New("123456:test-token",
		bot.WithSkipGetMe(),
		bot.WithNotAsyncHandlers(),
		bot.WithHTTPClient(time.Minute, stubHTTPClient{}),
	)
	if err != nil {
		t.Fatalf("build test bot: %v", err)
	}
	return b
}

// TestHandleMessageBlockedWhileCodexUnauthorized: with no completed Codex
// sign-in, a normal message must NOT reach the run Service — it would only fail
// deep inside the CLI, after paying for any transcription or download on the way.
func TestHandleMessageBlockedWhileCodexUnauthorized(t *testing.T) {
	auth, _ := unauthorizedCodexAuth(t)
	svc := &recordingSubmitter{}
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}
	deps := messageDeps{cfg: cfg, service: svc, auth: auth}

	handleMessage(context.Background(), deps, quietBot(t), textMessage("build it"), false)

	if got := svc.seen(); len(got) != 0 {
		t.Errorf("submitted %v while Codex was unauthorized, want nothing", got)
	}
}

// TestHandleMessageUnblocksAfterLogin: once the login has persisted auth.json the
// standard flow continues — in the SAME process, with no restart. That is the
// whole point of re-checking the file rather than caching a startup verdict.
func TestHandleMessageUnblocksAfterLogin(t *testing.T) {
	auth, home := unauthorizedCodexAuth(t)
	svc := &recordingSubmitter{}
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}
	deps := messageDeps{cfg: cfg, service: svc, auth: auth}
	b := quietBot(t)

	handleMessage(context.Background(), deps, b, textMessage("build it"), false)
	if got := svc.seen(); len(got) != 0 {
		t.Fatalf("submitted %v before the login, want nothing", got)
	}

	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	// A distinct prompt, so the assertion cannot pass on the blocked one leaking
	// through late.
	handleMessage(context.Background(), deps, b, textMessage("ship it"), false)
	if got := svc.seen(); len(got) != 1 || got[0] != "ship it" {
		t.Errorf("submitted %v after the login, want ['ship it']", got)
	}
}

// TestHandleMessageWithoutCodexAuthManager: the Claude default wires no manager,
// which must never block a message.
func TestHandleMessageWithoutCodexAuthManager(t *testing.T) {
	svc := &recordingSubmitter{}
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}
	deps := messageDeps{cfg: cfg, service: svc}

	handleMessage(context.Background(), deps, quietBot(t), textMessage("build it"), false)

	if got := svc.seen(); len(got) != 1 {
		t.Errorf("submitted %v with no auth manager, want the message through", got)
	}
}

// TestLoginCommandIsRoutedToItsHandler: /login is reserved, so it must be served
// by the bot and never forwarded to the model as a prompt — and it must stay
// reachable while the unauthorized gate is closed, since it is the way out.
func TestLoginCommandIsRoutedToItsHandler(t *testing.T) {
	auth, _ := unauthorizedCodexAuth(t)
	svc := &recordingSubmitter{}
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}

	defaultHandler := func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if msg := update.Message; msg != nil {
			handleMessage(ctx, messageDeps{cfg: cfg, service: svc, auth: auth}, b, msg, false)
		}
	}
	b, err := bot.New("123456:test-token",
		bot.WithSkipGetMe(),
		bot.WithNotAsyncHandlers(),
		bot.WithHTTPClient(time.Minute, stubHTTPClient{}),
		bot.WithDefaultHandler(defaultHandler),
	)
	if err != nil {
		t.Fatalf("build test bot: %v", err)
	}
	for name, h := range reservedHandlers(cfg, nil, nil, auth) {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}

	b.ProcessUpdate(context.Background(), privateCommandUpdate(loginTestUserID, loginTestChatID, "/login status", len("/login")))

	if got := svc.seen(); len(got) != 0 {
		t.Errorf("/login leaked to the model as %v, want it handled by the bot", got)
	}
}

// TestLoginIsAReservedCommand pins /login into the canonical set, which is what
// makes both adapters intercept it and Telegram publish it in the command menu.
func TestLoginIsAReservedCommand(t *testing.T) {
	if !chat.IsReservedCommand("login") {
		t.Fatal("login is not a reserved command; it would be forwarded to the model")
	}
}

// groupCommandUpdate builds a GROUP-chat slash command from the allowed sender.
func groupCommandUpdate(text string, cmdLen int) *models.Update {
	return &models.Update{Message: &models.Message{
		ID:       1,
		Text:     text,
		From:     &models.User{ID: loginTestUserID},
		Chat:     models.Chat{ID: loginTestChatID, Type: models.ChatTypeGroup},
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: cmdLen}},
	}}
}

// TestLoginRefusedInGroupChat: the reply goes to the CHAT, so in a group the
// verification link and one-time code would be readable by every member,
// allow-listed or not — and whoever acts on the code first binds THEIR ChatGPT
// account to the bot. The refusal must happen before any CLI is spawned.
func TestLoginRefusedInGroupChat(t *testing.T) {
	// A deliberately unusable CODEX_BIN: if the group check failed to short-circuit,
	// the manager would try to run it and the test's assertions would still pass —
	// so the manager is also asserted to have started nothing.
	home := t.TempDir()
	auth := codexauth.NewManager(codexauth.Config{
		Backend:     codexauth.BackendCodex,
		AuthMode:    codexauth.AuthSubscription,
		RequireAuth: true,
		Home:        home,
		Bin:         filepath.Join(home, "no-such-codex"),
	})
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}

	replies := &capturingHTTPClient{}
	b, err := bot.New("123456:test-token",
		bot.WithSkipGetMe(),
		bot.WithNotAsyncHandlers(),
		bot.WithHTTPClient(time.Minute, replies),
	)
	if err != nil {
		t.Fatalf("build test bot: %v", err)
	}
	for name, h := range reservedHandlers(cfg, nil, nil, auth) {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}

	b.ProcessUpdate(context.Background(), groupCommandUpdate("/login", len("/login")))

	got := replies.seen()
	if len(got) != 1 {
		t.Fatalf("replies = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "direct message") {
		t.Errorf("reply = %q, want the direct-message refusal", got[0])
	}
	if strings.Contains(got[0], "http") {
		t.Errorf("reply = %q leaked a link into the group", got[0])
	}
	if auth.StatusText() != "Codex is NOT authorized. Send /login to sign in with your ChatGPT account." {
		t.Errorf("a sign-in was started from a group chat: %q", auth.StatusText())
	}
}

// capturingHTTPClient answers every Bot API call locally like stubHTTPClient,
// but records the raw payload of each sendMessage so a test can assert what the
// user was actually told (the encoding varies, so assertions substring-match).
type capturingHTTPClient struct {
	mu    sync.Mutex
	texts []string
}

func (c *capturingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil && strings.Contains(req.URL.Path, "sendMessage") {
		body, _ := io.ReadAll(req.Body)
		c.mu.Lock()
		c.texts = append(c.texts, string(body))
		c.mu.Unlock()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)),
		Header:     make(http.Header),
	}, nil
}

func (c *capturingHTTPClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...)
}

// TestLoginStatusAllowedInGroupChat: status reveals no code, so refusing it
// everywhere would be noise rather than protection. It must answer with the real
// state, not the direct-message refusal.
func TestLoginStatusAllowedInGroupChat(t *testing.T) {
	auth, _ := unauthorizedCodexAuth(t)
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}
	http := &capturingHTTPClient{}

	b, err := bot.New("123456:test-token",
		bot.WithSkipGetMe(),
		bot.WithNotAsyncHandlers(),
		bot.WithHTTPClient(time.Minute, http),
	)
	if err != nil {
		t.Fatalf("build test bot: %v", err)
	}
	for name, h := range reservedHandlers(cfg, nil, nil, auth) {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}

	b.ProcessUpdate(context.Background(), groupCommandUpdate("/login status", len("/login")))

	got := http.seen()
	if len(got) != 1 {
		t.Fatalf("replies = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "NOT authorized") {
		t.Errorf("reply = %q, want the status, not a refusal", got[0])
	}
}
