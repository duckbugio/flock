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

	"github.com/duckbugio/flock/adapters/telegram"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/codexauth"
	"github.com/duckbugio/flock/core/codexauth/codexauthtest"
	"github.com/duckbugio/flock/core/codexauth/logintest"
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
		// A deliberately absent binary, so no case can start the REAL codex and sit
		// polling for the full DeviceCodeTTL.
		Bin: filepath.Join(home, "no-such-codex"),
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
	// A deliberately unusable CODEX_BIN: if the refusal failed to short-circuit,
	// the manager would try to run it, which the status assertion below catches.
	home := t.TempDir()
	auth := codexauth.NewManager(codexauth.Config{
		Backend:     codexauth.BackendCodex,
		AuthMode:    codexauth.AuthSubscription,
		RequireAuth: true,
		Home:        home,
		Bin:         filepath.Join(home, "no-such-codex"),
	})

	got := groupCommandReplies(t, auth, "/login")
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

// TestLoginStatusInGroupHidesAPendingCode is the regression for the hole an
// argument-based guard left: /login status ALSO re-shows a pending one-time code,
// so refusing only the code-starting arguments let it into the group anyway.
func TestLoginStatusInGroupHidesAPendingCode(t *testing.T) {
	auth := logintest.PendingLogin(t)

	got := groupCommandReplies(t, auth, "/login status")
	if len(got) != 1 {
		t.Fatalf("replies = %v, want exactly one", got)
	}
	if strings.Contains(got[0], codexauthtest.DeviceCode) || strings.Contains(got[0], "http") {
		t.Errorf("the pending code or link reached a group: %q", got[0])
	}
	if !strings.Contains(got[0], "in progress") {
		t.Errorf("status = %q, want it to still report the pending sign-in", got[0])
	}
}

// TestLoginStatusInPrivateShowsThePendingCode: the same command in a direct
// message must still re-show the code — that is what makes a lost message
// recoverable.
func TestLoginStatusInPrivateShowsThePendingCode(t *testing.T) {
	auth := logintest.PendingLogin(t)
	cfg := config.Config{AllowedUsers: []int64{loginTestUserID}}
	replies := &capturingHTTPClient{}
	b := commandBot(t, cfg, auth, replies)

	b.ProcessUpdate(context.Background(),
		privateCommandUpdate(loginTestUserID, loginTestChatID, "/login status", len("/login")))

	got := replies.seen()
	if len(got) != 1 || !strings.Contains(got[0], codexauthtest.DeviceCode) {
		t.Errorf("replies = %v, want the pending code re-shown in a direct message", got)
	}
}

// commandBot builds a bot with the reserved handlers registered and every API
// call answered locally by http.
func commandBot(t *testing.T, cfg config.Config, auth *codexauth.Manager, client bot.HttpClient) *bot.Bot {
	t.Helper()
	b, err := bot.New("123456:test-token",
		bot.WithSkipGetMe(),
		bot.WithNotAsyncHandlers(),
		bot.WithHTTPClient(time.Minute, client),
	)
	if err != nil {
		t.Fatalf("build test bot: %v", err)
	}
	for name, h := range reservedHandlers(cfg, nil, nil, auth) {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}
	return b
}

// groupCommandReplies runs text as a group slash command and returns what the
// bot replied.
func groupCommandReplies(t *testing.T, auth *codexauth.Manager, text string) []string {
	t.Helper()
	replies := &capturingHTTPClient{}
	b := commandBot(t, config.Config{AllowedUsers: []int64{loginTestUserID}}, auth, replies)
	b.ProcessUpdate(context.Background(), groupCommandUpdate(text, len("/login")))
	return replies.seen()
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

// TestLoginStatusInGroupWithNothingPending: with no sign-in in flight there is
// no code to withhold, so the group is simply told the state.
func TestLoginStatusInGroupWithNothingPending(t *testing.T) {
	auth, _ := unauthorizedCodexAuth(t)

	got := groupCommandReplies(t, auth, "/login status")
	if len(got) != 1 {
		t.Fatalf("replies = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "NOT authorized") {
		t.Errorf("reply = %q, want the status, not a refusal", got[0])
	}
}

// codexAuthForMenu is a manager for a Codex subscription deploy, i.e. one where
// every reserved command — including /login — applies.
var codexAuthForMenu = codexauth.NewManager(codexauth.Config{
	Backend:  codexauth.BackendCodex,
	AuthMode: codexauth.AuthSubscription,
})

// TestMenuOmitsLoginWithoutAnInteractiveSignIn: on Claude, an API-key backend or
// Codex billing there is no sign-in to complete, so advertising /login in the
// menu would only lead a user to a command that answers "not needed". The
// handler stays registered, so typing it still gets that answer.
func TestMenuOmitsLoginWithoutAnInteractiveSignIn(t *testing.T) {
	for _, tt := range []struct {
		name string
		auth *codexauth.Manager
	}{
		{"claude", codexauth.NewManager(codexauth.Config{Backend: "claude"})},
		{"codex billing", codexauth.NewManager(codexauth.Config{
			Backend: codexauth.BackendCodex, AuthMode: codexauth.AuthBilling,
		})},
		{"no manager", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, c := range reservedBotCommands(tt.auth) {
				if c.Command == "login" {
					t.Errorf("/login is published in the menu on a deployment with no sign-in")
				}
			}
			if got, want := len(reservedBotCommands(tt.auth)), len(chat.ReservedCommands)-1; got != want {
				t.Errorf("menu has %d entries, want %d (everything but /login)", got, want)
			}
		})
	}
}

// TestMenuKeepsLoginOnCodexSubscription is the other half: where the sign-in is
// real, the command must be advertised.
func TestMenuKeepsLoginOnCodexSubscription(t *testing.T) {
	found := false
	for _, c := range reservedBotCommands(codexAuthForMenu) {
		if c.Command == "login" {
			found = true
		}
	}
	if !found {
		t.Error("/login is missing from the menu on a Codex subscription deployment")
	}
}

// TestHelpListsLoginOnlyWhereItApplies keeps /help and the published command menu
// on the SAME condition. The menu hides /login where no sign-in exists; a help
// text that still advertises it would send the user to a command that can only
// answer "not needed".
func TestHelpListsLoginOnlyWhereItApplies(t *testing.T) {
	if !strings.Contains(telegram.HelpText(chat.WithLogin), "/login") {
		t.Error("help omits /login where the sign-in is real")
	}
	if strings.Contains(telegram.HelpText(chat.WithoutLogin), "/login") {
		t.Error("help advertises /login where there is no sign-in")
	}
	if !strings.Contains(telegram.WelcomeText(chat.WithLogin), "/login") {
		t.Error("welcome omits /login where the sign-in is real")
	}
	if strings.Contains(telegram.WelcomeText(chat.WithoutLogin), "/login") {
		t.Error("welcome advertises /login where there is no sign-in")
	}

	// The menu and the help must agree, which is the whole point of one condition.
	menuHasLogin := false
	for _, c := range reservedBotCommands(codexAuthForMenu) {
		if c.Command == loginCommand {
			menuHasLogin = true
		}
	}
	if menuHasLogin != strings.Contains(telegram.HelpText(chat.LoginVisibilityFor(codexAuthForMenu.Applicable())), "/login") {
		t.Error("the command menu and /help disagree about /login")
	}
}

// TestHelpLineComesFromTheCanonicalSet: both adapters render the same /login help
// line from core/chat, so a wording change cannot land in one and not the other.
func TestHelpLineComesFromTheCanonicalSet(t *testing.T) {
	if !strings.Contains(telegram.HelpText(chat.WithLogin), chat.LoginHelpLine) {
		t.Error("the Telegram help does not render the canonical /login line")
	}
}
