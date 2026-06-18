package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/adapters/telegram"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/schedule"
	"github.com/duckbugio/flock/internal/config"
)

// TestCommandSenderAllowList is the AC4 gate: only an allowed sender's slash
// command is acted on; a disallowed (or sender-less) command is silently ignored
// — the same treatment a disallowed text message gets, with no info leak.
func TestCommandSenderAllowList(t *testing.T) {
	cfg := config.Config{AllowedUsers: []int64{10, 20}}

	tests := []struct {
		name       string
		msg        *models.Message
		wantChatID int64
		wantOK     bool
	}{
		{
			name:       "allowed user",
			msg:        &models.Message{From: &models.User{ID: 10}, Chat: models.Chat{ID: 555}},
			wantChatID: 555,
			wantOK:     true,
		},
		{
			name:   "disallowed user ignored",
			msg:    &models.Message{From: &models.User{ID: 99}, Chat: models.Chat{ID: 555}},
			wantOK: false,
		},
		{
			name:   "sender-less message ignored",
			msg:    &models.Message{Chat: models.Chat{ID: 555}},
			wantOK: false,
		},
		{
			name:   "nil message ignored",
			msg:    nil,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chatID, ok := commandSender(cfg, tt.msg)
			if ok != tt.wantOK {
				t.Fatalf("commandSender ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && chatID != tt.wantChatID {
				t.Fatalf("commandSender chatID = %d, want %d", chatID, tt.wantChatID)
			}
		})
	}
}

// TestReservedBotCommandsFromCanonicalSet asserts the published Telegram command
// menu is built one-for-one from core/chat.ReservedCommands (the single source of
// truth) — same names, same order, same descriptions — so the menu can never
// drift from the commands the handlers route on.
func TestReservedBotCommandsFromCanonicalSet(t *testing.T) {
	got := reservedBotCommands()
	if len(got) != len(chat.ReservedCommands) {
		t.Fatalf("reservedBotCommands has %d entries, want %d", len(got), len(chat.ReservedCommands))
	}
	for i, c := range chat.ReservedCommands {
		if got[i].Command != c.Name {
			t.Errorf("command[%d] = %q, want %q", i, got[i].Command, c.Name)
		}
		if got[i].Description != c.Description {
			t.Errorf("command[%d] description = %q, want %q", i, got[i].Description, c.Description)
		}
	}
}

// botCommandUpdate builds an Update carrying a slash command, mirroring what
// Telegram sends (a bot_command entity at offset 0 spanning "/name[@botname]").
func botCommandUpdate(text string, cmdLen int) *models.Update {
	return &models.Update{Message: &models.Message{
		Text:     text,
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: cmdLen}},
	}}
}

// TestStartCommandRouted is the /start routing guard: /start must be matched as a command
// (so it is handled by the start handler and never reaches the text/model path),
// tolerating the group @botname suffix exactly like the other slash commands.
// Without a /start handler the message falls through to the default text handler
// and is forwarded to the model, which improvises a confusing reply.
func TestStartCommandRouted(t *testing.T) {
	match := commandMatch("start")
	tests := []struct {
		name string
		u    *models.Update
		want bool
	}{
		{"bare /start", botCommandUpdate("/start", 6), true},
		{"group form /start@duck_bot", botCommandUpdate("/start@duck_bot", 15), true},
		{"/start with args", botCommandUpdate("/start now", 6), true},
		{"different command /stop", botCommandUpdate("/stop", 5), false},
		{"nil update", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := match(tt.u); got != tt.want {
				t.Fatalf("commandMatch(start)(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestStartWelcomeText asserts the static /start reply is the welcome text: a
// short greeting prepended to the usage help, plain English with no duck flavor.
func TestStartWelcomeText(t *testing.T) {
	if !strings.HasPrefix(telegram.WelcomeText, "Hi!") {
		t.Fatalf("WelcomeText should open with a greeting, got %q", telegram.WelcomeText)
	}
	if !strings.Contains(telegram.WelcomeText, telegram.HelpText) {
		t.Fatalf("WelcomeText should include the usage help (HelpText)")
	}
}

// TestReservedHandlersMatchCanonicalSet is the drift guard for Finding 2: the set
// of command names the bot registers reserved handlers for (reservedHandlers) MUST
// equal the canonical chat.ReservedCommands. The menu is published from the
// canonical set (reservedBotCommands); if a new reserved command were added there
// but not given a handler here it would be advertised in the menu yet leak to
// Claude as free text. Keying both off the same source and asserting equality makes
// that impossible.
func TestReservedHandlersMatchCanonicalSet(t *testing.T) {
	// A nil *chat.Service and nil *schedule.Manager are fine: we only inspect the
	// map's key set, never invoke a handler closure.
	handlers := reservedHandlers(config.Config{}, nil, nil)

	if len(handlers) != len(chat.ReservedCommands) {
		t.Fatalf("reservedHandlers has %d entries, want %d (chat.ReservedCommands)",
			len(handlers), len(chat.ReservedCommands))
	}
	for _, c := range chat.ReservedCommands {
		h, ok := handlers[c.Name]
		if !ok {
			t.Errorf("no handler registered for reserved command %q", c.Name)
			continue
		}
		if h == nil {
			t.Errorf("handler for reserved command %q is nil", c.Name)
		}
	}
	// And no EXTRA handler is registered for a name that is not reserved (which would
	// silently intercept a command the user expects to pass through to Claude).
	reserved := map[string]bool{}
	for _, c := range chat.ReservedCommands {
		reserved[c.Name] = true
	}
	for name := range handlers {
		if !reserved[name] {
			t.Errorf("handler registered for %q, which is not in chat.ReservedCommands", name)
		}
	}
}

// recordingSubmitter is a messageSubmitter that records every submit call, so a
// test can assert exactly what reached the run Service via the message path. It
// mirrors the VK receiver test's fakeService seam.
type recordingSubmitter struct {
	mu      sync.Mutex
	prompts []string
}

func (r *recordingSubmitter) Handle(_ context.Context, _ chat.ChatID, _ int64, _ chat.MessageID, prompt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
}

func (r *recordingSubmitter) HandleEdit(_ context.Context, _ chat.ChatID, _ int64, _ chat.MessageID, prompt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
}

func (r *recordingSubmitter) HandleMedia(
	_ context.Context, _ chat.ChatID, _ int64, _ chat.MessageID, prompt string, _ []claude.ImageInput,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
}

func (r *recordingSubmitter) HandleEditMedia(
	_ context.Context, _ chat.ChatID, _ int64, _ chat.MessageID, prompt string, _ []claude.ImageInput,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
}

func (r *recordingSubmitter) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

// stubHTTPClient is a bot HttpClient that answers every request locally with a
// canned ok=true envelope, so a handler that posts a message (e.g. the /stop
// confirmation) is a clean no-op with no network. It lets the test exercise the
// REAL reserved handlers without hitting api.telegram.org.
type stubHTTPClient struct{}

func (stubHTTPClient) Do(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
		Header:     make(http.Header),
	}, nil
}

// newCommandTestBot builds a bot that dispatches updates exactly as production does
// — the reserved-command handlers match BEFORE the default text handler — but with
// the run Service swapped for a recording fake on the pass-through path. It skips
// getMe and stubs the HTTP client (no network) and runs handlers synchronously so a
// single ProcessUpdate is fully handled before we assert. The token's numeric prefix
// becomes the bot's own user id (used by the mention gate); the value is otherwise
// irrelevant. reservedSvc backs the REAL reserved handlers (e.g. /stop's StopChat);
// it is a no-op service over a real dispatcher so the handlers run without panicking.
func newCommandTestBot(t *testing.T, cfg config.Config, svc messageSubmitter, reservedSvc *chat.Service) *bot.Bot {
	t.Helper()
	defaultHandler := func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if msg := update.Message; msg != nil {
			deps := messageDeps{cfg: cfg, service: svc}
			handleMessage(ctx, deps, b, msg, false)
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
	// Register the reserved-command handlers from the SAME canonical source main uses,
	// so the routing (reserved handler vs default) is identical to production. The
	// scheduler is nil here (the pass-through tests do not exercise /schedule).
	for name, h := range reservedHandlers(cfg, reservedSvc, nil) {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}
	return b
}

// privateCommandUpdate builds a PRIVATE-chat update carrying a slash command from
// the given user, mirroring what Telegram sends (a bot_command entity at offset 0).
func privateCommandUpdate(userID, chatID int64, text string, cmdLen int) *models.Update {
	return &models.Update{Message: &models.Message{
		ID:       1,
		Text:     text,
		From:     &models.User{ID: userID},
		Chat:     models.Chat{ID: chatID, Type: models.ChatTypePrivate},
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: cmdLen}},
	}}
}

// TestNativeCommandPassesThroughTelegram is the Telegram parity for AC-A2 (the VK
// analog is TestReceiverNativeCommandPassesThrough): a non-reserved slash command
// in a private chat reaches the run Service via the message path with its FULL text
// verbatim, while a reserved command (/stop) is routed to its own handler and never
// reaches the Service this way.
func TestNativeCommandPassesThroughTelegram(t *testing.T) {
	const userID = 42
	cfg := config.Config{AllowedUsers: []int64{userID}}
	svc := &recordingSubmitter{}
	// A no-op real Service backs the reserved handlers so /stop's StopChat runs
	// without panicking; the pass-through path uses the recording fake (svc).
	reservedSvc := chat.New(chat.Config{Dispatcher: dispatch.New(1)})
	b := newCommandTestBot(t, cfg, svc, reservedSvc)

	// A native (non-reserved) command falls through to the default handler and is
	// submitted verbatim — "/loop 5m /foo", the whole text, not just "/loop".
	const native = "/loop 5m /foo"
	b.ProcessUpdate(context.Background(), privateCommandUpdate(userID, 200, native, len("/loop")))

	if got := svc.seen(); len(got) != 1 || got[0] != native {
		t.Fatalf("native command not passed through verbatim: Service saw %v, want [%q]", got, native)
	}

	// A reserved command is matched by its own handler BEFORE the default text
	// handler, so it must NOT reach the Service via this path.
	b.ProcessUpdate(context.Background(), privateCommandUpdate(userID, 200, "/stop", len("/stop")))

	if got := svc.seen(); len(got) != 1 {
		t.Fatalf("reserved /stop leaked to the run Service: Service saw %v, want only the native command", got)
	}
}

// TestScheduleCommandDisabled: with no scheduler wired (mgr nil), /schedule is
// routed to its own handler (never the default text/Claude path) and the handler
// replies with the disabled notice. We assert routing by checking the pass-through
// Service never sees the command.
func TestScheduleCommandDisabled(t *testing.T) {
	const userID = 42
	cfg := config.Config{AllowedUsers: []int64{userID}}
	svc := &recordingSubmitter{}
	b := newCommandTestBot(t, cfg, svc, nil) // reservedSvc nil: /schedule needs no Service

	b.ProcessUpdate(context.Background(), privateCommandUpdate(userID, 555, "/schedule list", len("/schedule")))

	if got := svc.seen(); len(got) != 0 {
		t.Fatalf("/schedule leaked to the run Service when disabled: saw %v", got)
	}
}

// TestScheduleCommandEnabledReachesDispatch: with a scheduler wired into the
// reserved handlers, "/schedule add ..." reaches Manager.Dispatch (the job is
// persisted) and never reaches the pass-through Service.
func TestScheduleCommandEnabledReachesDispatch(t *testing.T) {
	const userID = 77 // a distinct sender so CreatedBy is asserted meaningfully
	cfg := config.Config{AllowedUsers: []int64{userID}}
	svc := &recordingSubmitter{}

	store, err := schedule.Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	mgr := schedule.NewManager(store, func(string, string) {}, nil, nil)

	defaultHandler := func(ctx context.Context, b *bot.Bot, update *models.Update) {
		if msg := update.Message; msg != nil {
			deps := messageDeps{cfg: cfg, service: svc}
			handleMessage(ctx, deps, b, msg, false)
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
	for name, h := range reservedHandlers(cfg, nil, mgr) {
		b.RegisterHandlerMatchFunc(commandMatch(name), h)
	}

	b.ProcessUpdate(context.Background(), privateCommandUpdate(
		userID, 200, "/schedule add job 0 9 * * 1 do the thing", len("/schedule")))

	if got := svc.seen(); len(got) != 0 {
		t.Fatalf("/schedule leaked to the run Service when enabled: saw %v", got)
	}
	jobs := store.List("200")
	if len(jobs) != 1 || jobs[0].Prompt != "do the thing" {
		t.Fatalf("store after /schedule add = %+v, want one job with prompt 'do the thing'", jobs)
	}
	if jobs[0].CreatedBy != userID {
		t.Errorf("job CreatedBy = %d, want the sender %d", jobs[0].CreatedBy, userID)
	}
}

// TestCommandArgs asserts the /schedule arg extractor strips the leading command
// token (with or without an @botname suffix) and trims surrounding whitespace.
func TestCommandArgs(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"/schedule list", "list"},
		{"/schedule add a 0 9 * * 1 do it", "add a 0 9 * * 1 do it"},
		{"/schedule@duck_bot list", "list"},
		{"/schedule", ""},
		{"/schedule   ", ""},
		{"  /schedule  list  ", "list"},
	}
	for _, tt := range tests {
		if got := commandArgs(tt.text); got != tt.want {
			t.Errorf("commandArgs(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

// TestCommandMatchToleratesBotnameSuffix is the routing guard: a command handler
// must match both /new and the group form /new@duck_bot (Telegram auto-inserts
// the @botname suffix in groups), while NOT matching a different command or a
// nil update. Without this the group form would fall through to the text handler
// and be forwarded to the model.
func TestCommandMatchToleratesBotnameSuffix(t *testing.T) {
	match := commandMatch("new")
	tests := []struct {
		name string
		u    *models.Update
		want bool
	}{
		{"bare /new", botCommandUpdate("/new", 4), true},
		{"group form /new@duck_bot", botCommandUpdate("/new@duck_bot", 13), true},
		{"/new with args", botCommandUpdate("/new please", 4), true},
		{"different command /news", botCommandUpdate("/news", 5), false},
		{"nil update", nil, false},
		{"update without message", &models.Update{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := match(tt.u); got != tt.want {
				t.Fatalf("commandMatch(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
