//nolint:testpackage // intentionally whitebox to test unexported telegram command handlers
package telegram

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot/models"
)

// botCmd builds a message whose leading token is a bot_command entity, the shape
// Telegram produces for a slash command (the entity spans "/name[@botname]").
func botCmd(text string, cmdLen int) *models.Message {
	return &models.Message{
		Text:     text,
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: cmdLen}},
	}
}

// TestCommandName asserts the command parser strips the @botname suffix Telegram
// adds in groups (so /new and /new@duck_bot are equivalent), ignores arguments
// and non-commands, and does not treat a "/new" appearing mid-message as a
// command — the routing fix that keeps group commands from leaking to the model.
func TestCommandName(t *testing.T) {
	tests := []struct {
		name string
		msg  *models.Message
		want string
	}{
		{"bare command", botCmd("/new", 4), "new"},
		{"command with @botname (group form)", botCmd("/new@duck_bot", 13), "new"},
		{"command with args", botCmd("/new please", 4), "new"},
		{"command with @botname and args", botCmd("/stop@duck_bot now", 14), "stop"},
		{"different command not confused", botCmd("/news", 5), "news"},
		{"plain text is not a command", &models.Message{Text: "hello there"}, ""},
		{"mid-text slash is not a command", &models.Message{
			Text:     "say /new",
			Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 4, Length: 4}},
		}, ""},
		{"nil message", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommandName(tc.msg); got != tc.want {
				t.Fatalf("CommandName = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeDispatcher records Submit and Cancel calls without running anything, so a
// command test can assert that /help never submits a run and that /stop cancels
// the right chat. It satisfies the dispatcher interface.
type fakeDispatcher struct {
	mu        sync.Mutex
	submitted []int64 // chatIDs passed to Submit, in order
	cancelled []int64 // chatIDs passed to Cancel, in order
}

func (d *fakeDispatcher) Submit(chatID int64, _ func(ctx context.Context)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.submitted = append(d.submitted, chatID)
}

func (d *fakeDispatcher) Cancel(chatID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancelled = append(d.cancelled, chatID)
}

func (d *fakeDispatcher) submits() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int64(nil), d.submitted...)
}

func (d *fakeDispatcher) cancels() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int64(nil), d.cancelled...)
}

// newCommandService builds a Service over a fake dispatcher + the given session
// store so command tests can assert on Submit/Cancel/Delete without a live run.
func newCommandService(disp dispatcher, sess sessionStore) *Service {
	return New(Config{
		Runner:     &fakeRunner{},
		Chat:       newFakeChat(),
		Dispatcher: disp,
		Workspace:  &fakeWorkspace{},
		Sessions:   sess,
		Logger:     slog.New(slog.DiscardHandler),
	})
}

// TestHelpTextListsCommands asserts the static /help payload exists and lists all
// three slash commands — AC1. /help is a pure reply (it has no Service method, so
// it can never submit a run); the constant IS the entire payload the handler
// sends.
func TestHelpTextListsCommands(t *testing.T) {
	if HelpText == "" {
		t.Fatal("HelpText is empty")
	}
	for _, cmd := range []string{"/help", "/new", "/stop"} {
		if !strings.Contains(HelpText, cmd) {
			t.Fatalf("HelpText does not mention %q:\n%s", cmd, HelpText)
		}
	}
}

// TestNewSessionResets asserts /new deletes the chat's stored session (so the
// next message starts fresh) and that a reset of an absent chat is a no-op that
// still succeeds — AC2.
func TestNewSessionResets(t *testing.T) {
	disp := &fakeDispatcher{}
	sess := newFakeSessions()
	if err := sess.Set(100, "sess-seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := newCommandService(disp, sess)

	if err := svc.NewSession(100); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, ok := sess.get(); ok {
		t.Fatal("session 100 still present after NewSession")
	}
	if got := sess.deletes(); len(got) != 1 || got[0] != 100 {
		t.Fatalf("Delete log = %v, want [100]", got)
	}

	// Resetting an absent chat is a harmless no-op that still returns nil.
	if err := svc.NewSession(999); err != nil {
		t.Fatalf("NewSession on absent chat: %v", err)
	}

	// /new must not submit a run (no Claude invocation).
	if got := disp.submits(); len(got) != 0 {
		t.Fatalf("NewSession submitted runs: %v", got)
	}
}

// TestNewSessionNilStore asserts /new is a safe no-op when continuity is disabled
// (nil store): no panic, no error — AC2.
func TestNewSessionNilStore(t *testing.T) {
	disp := &fakeDispatcher{}
	svc := newCommandService(disp, nil)
	if err := svc.NewSession(100); err != nil {
		t.Fatalf("NewSession with nil store: %v", err)
	}
}

// TestStopChatCancels asserts /stop routes to dispatch.Cancel(chatID) and reports
// whether a run was active — AC3. With a registered in-flight run it returns true
// ("stopping" copy); with none it returns false ("nothing to stop") yet still
// issues the (no-op) Cancel via the SAME primitive as the inline Stop button.
func TestStopChatCancels(t *testing.T) {
	disp := &fakeDispatcher{}
	svc := newCommandService(disp, newFakeSessions())

	// No run in flight: StopChat reports false (nothing to stop) but still calls
	// Cancel(chatID) — a single, shared cancel path.
	if svc.StopChat(100) {
		t.Fatal("StopChat reported a run active when none was registered")
	}
	if got := disp.cancels(); len(got) != 1 || got[0] != 100 {
		t.Fatalf("Cancel log = %v, want [100]", got)
	}

	// Register an in-flight run for chat 100 (mimic run()'s bookkeeping).
	svc.mu.Lock()
	svc.runChat["7"] = 100
	svc.mu.Unlock()

	if !svc.StopChat(100) {
		t.Fatal("StopChat reported no run active when one was registered")
	}
	if got := disp.cancels(); len(got) != 2 || got[1] != 100 {
		t.Fatalf("Cancel log = %v, want second cancel of 100", got)
	}
}
