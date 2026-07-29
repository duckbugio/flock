package main

import (
	"context"
	"os"
	"path/filepath"
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
