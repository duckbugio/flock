//nolint:testpackage // intentionally whitebox to test the device-login parser and manager internals
package codexauth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeAuthFile plants the auth.json a completed Codex login leaves behind.
func writeAuthFile(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

// collector accumulates the background notices a login delivers.
type collector struct {
	ch chan string
}

func newCollector() *collector { return &collector{ch: make(chan string, 8)} }

// persistAuth is the shell step a fake CLI uses to stand in for the browser
// half of the flow: a real successful login writes auth.json into CODEX_HOME,
// and that file — not the exit code — is what authorizes later runs.
const persistAuth = "sleep 0.3\necho '{}' > \"$CODEX_HOME/auth.json\"\n"

// dmSub is a private (1:1) destination — the only kind allowed to see a code.
var dmSub = Subscriber{ID: "dm", Private: true}

func (c *collector) notify(text string) { c.ch <- text }

// sub is the collector as a Subscriber for destination id.
func (c *collector) sub(id string) Subscriber {
	return Subscriber{ID: id, Private: true, Notify: c.notify}
}

// awaitCode drains notices until the one carrying the device code, returning it.
// The immediate acknowledgement now arrives through the SAME subscriber (that
// ordering is the point), so a test that only wants the prompt must skip past it.
func (c *collector) awaitCode(t *testing.T) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case text := <-c.ch:
			if strings.Contains(text, deviceCode) {
				return text
			}
		case <-deadline:
			t.Fatal("no device code within 10s")
			return ""
		}
	}
}

// next returns the next notice, failing the test if none arrives in time.
func (c *collector) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-c.ch:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no login notice within 10s")
		return ""
	}
}

// TestApplicableOnlyForCodexSubscription: every other setup authenticates from
// env alone, so /login must say so instead of spawning a CLI.
func TestApplicableOnlyForCodexSubscription(t *testing.T) {
	tests := []struct {
		name     string
		backend  string
		authMode string
		want     bool
	}{
		{"codex subscription", BackendCodex, AuthSubscription, true},
		{"codex subscription mixed case", "Codex", "Subscription", true},
		{"codex billing", BackendCodex, AuthBilling, false},
		{"claude", "claude", AuthSubscription, false},
		{"openai-compatible", "openai-compat", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(Config{Backend: tt.backend, AuthMode: tt.authMode})
			if got := m.Applicable(); got != tt.want {
				t.Errorf("Applicable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuthorizedSources: a persisted auth.json or a configured access token each
// authorize Codex; neither present means the deployment needs a login.
func TestAuthorizedSources(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir()})
		if m.Authorized() {
			t.Error("Authorized() = true with no auth.json and no access token")
		}
		if !m.NeedsLogin() {
			t.Error("NeedsLogin() = false, want true")
		}
	})
	t.Run("persisted auth file", func(t *testing.T) {
		home := t.TempDir()
		writeAuthFile(t, home)
		m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: home})
		if !m.Authorized() {
			t.Error("Authorized() = false with a persisted auth.json")
		}
	})
	t.Run("access token", func(t *testing.T) {
		m := NewManager(Config{
			Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir(), HasAccessToken: true,
		})
		if !m.Authorized() {
			t.Error("Authorized() = false with CODEX_ACCESS_TOKEN configured")
		}
	})
	t.Run("non-codex backend never needs a login", func(t *testing.T) {
		m := NewManager(Config{Backend: "claude"})
		if !m.Authorized() {
			t.Error("Authorized() = false for a non-Codex backend")
		}
	})
}

// TestAuthorizedRechecksTheAuthFile is what lets the very first message after a
// successful /login run WITHOUT a restart: the verdict is re-read from disk, not
// cached from startup.
func TestAuthorizedRechecksTheAuthFile(t *testing.T) {
	home := t.TempDir()
	m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: home})
	if m.Authorized() {
		t.Fatal("Authorized() = true before any login")
	}
	writeAuthFile(t, home)
	if !m.Authorized() {
		t.Error("Authorized() = false after auth.json appeared; the verdict was cached")
	}
}

// TestBlockedNotice: an unauthorized deployment blocks runs with a notice naming
// the one command that fixes it, and never blocks once authorized.
func TestBlockedNotice(t *testing.T) {
	home := t.TempDir()
	m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: home})

	notice, blocked := m.BlockedNotice()
	if !blocked {
		t.Fatal("BlockedNotice() reported no block while unauthorized")
	}
	if !strings.Contains(notice, "/login") {
		t.Errorf("notice %q should tell the user to send /login", notice)
	}

	writeAuthFile(t, home)
	if _, blocked := m.BlockedNotice(); blocked {
		t.Error("BlockedNotice() still blocks after a completed login")
	}
}

// TestNilManagerIsInert: an adapter wired without a Manager must degrade to "no
// login needed" rather than panic.
func TestNilManagerIsInert(t *testing.T) {
	var m *Manager
	if m.Applicable() {
		t.Error("nil Manager reported Applicable")
	}
	if !m.Authorized() {
		t.Error("nil Manager reported unauthorized")
	}
	if _, blocked := m.BlockedNotice(); blocked {
		t.Error("nil Manager blocked a run")
	}
	if m.Cancel() {
		t.Error("nil Manager cancelled a login")
	}
	if got := m.Dispatch(context.Background(), "", Subscriber{ID: "dm", Private: true}); got != NoLoginNeededText {
		t.Errorf("nil Manager Dispatch = %q, want NoLoginNeededText", got)
	}
}

// TestDispatchNotApplicable: /login on a Claude or billing deployment explains
// itself instead of running anything.
func TestDispatchNotApplicable(t *testing.T) {
	claude := NewManager(Config{Backend: "claude"})
	if got := claude.Dispatch(context.Background(), "", Subscriber{ID: "dm", Private: true}); !strings.Contains(got, "claude") {
		t.Errorf("Dispatch = %q, want it to name the configured backend", got)
	}
	billing := NewManager(Config{Backend: BackendCodex, AuthMode: AuthBilling})
	dm := Subscriber{ID: "dm", Private: true}
	if got := billing.Dispatch(context.Background(), "", dm); !strings.Contains(got, "CODEX_API_KEY") {
		t.Errorf("Dispatch = %q, want the billing-mode explanation", got)
	}
}

// TestDispatchFullLoginPath walks the whole user-visible journey the feature
// exists for: an unauthorized deployment blocks runs, /login hands back a link
// and a one-time code while the CLI is still polling, the completed sign-in is
// confirmed, and the standard flow is unblocked — no restart involved.
func TestDispatchFullLoginPath(t *testing.T) {
	home := t.TempDir()
	// The fake CLI prints the real banner, then "completes" the browser step by
	// writing the auth.json a real login would persist.
	bin := fakeCodex(t, printBanner+"sleep 0.2\necho '{}' > \"$CODEX_HOME/auth.json\"\nexit 0\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        home,
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	if _, blocked := m.BlockedNotice(); !blocked {
		t.Fatal("runs were not blocked before the login")
	}

	c := newCollector()
	if reply := m.Dispatch(context.Background(), "", c.sub("chat-1")); reply != "" {
		t.Errorf("immediate reply = %q, want it delivered through the subscriber instead", reply)
	}

	// Order matters: the acknowledgement must not surface UNDER the code it
	// promises, which is what sending it outside this channel used to risk.
	if ack := c.next(t); !strings.Contains(ack, "Starting") {
		t.Errorf("first notice = %q, want the acknowledgement first", ack)
	}
	prompt := c.next(t)
	if !strings.Contains(prompt, "https://auth.openai.com/codex/device") {
		t.Errorf("prompt %q is missing the verification link", prompt)
	}
	if !strings.Contains(prompt, "XER9-NWCA2") {
		t.Errorf("prompt %q is missing the one-time code", prompt)
	}
	if !strings.Contains(prompt, "15 min") {
		t.Errorf("prompt %q is missing the code lifetime", prompt)
	}

	done := c.next(t)
	if !strings.Contains(strings.ToLower(done), "authorized") {
		t.Errorf("final notice = %q, want a success confirmation", done)
	}
	if _, blocked := m.BlockedNotice(); blocked {
		t.Error("runs are still blocked after a successful login")
	}
}

// TestDispatchReshowsPendingPrompt: a second /login while one is pending must
// re-show the SAME code, not start a competing flow that would invalidate it.
func TestDispatchReshowsPendingPrompt(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	bin := fakeCodex(t, "echo x >> "+runs+"\n"+printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("chat-1"))
	c.awaitCode(t)

	// The re-show goes through the subscriber (so a broadcast cannot overtake it),
	// which is why Dispatch itself returns nothing for the adapter to send.
	if again := m.Dispatch(context.Background(), "", c.sub("chat-1")); again != "" {
		t.Errorf("second /login returned %q, want it delivered through the subscriber", again)
	}
	reshow := c.next(t)
	if !strings.Contains(reshow, deviceCode) {
		t.Errorf("re-show = %q, want the pending code", reshow)
	}
	if !strings.Contains(reshow, "already pending") {
		t.Errorf("re-show = %q, want it to say a login is already pending", reshow)
	}

	//nolint:gosec // the path is a test-owned temp file
	data, err := os.ReadFile(runs)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	if got := strings.Count(string(data), "x"); got != 1 {
		t.Errorf("the CLI ran %d times, want 1: a second login would invalidate the first code", got)
	}
}

// TestDispatchCancel aborts a pending sign-in and reports the cancellation.
func TestDispatchCancel(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	if got := m.Dispatch(context.Background(), "cancel", dmSub); !strings.Contains(got, "No Codex login is pending") {
		t.Errorf("cancel with nothing pending = %q", got)
	}

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("chat-1"))
	c.awaitCode(t)

	if got := m.Dispatch(context.Background(), "cancel", c.sub("chat-1")); !strings.Contains(got, "Cancelled") {
		t.Errorf("cancel = %q, want a cancellation acknowledgement", got)
	}
	if got := c.next(t); !strings.Contains(got, "cancelled") {
		t.Errorf("final notice = %q, want the cancellation reported", got)
	}
}

// TestDispatchAlreadyAuthorized: /login on an authorized deployment reports the
// state and offers the explicit re-login rather than silently replacing a
// working credential.
func TestDispatchAlreadyAuthorized(t *testing.T) {
	home := t.TempDir()
	writeAuthFile(t, home)
	m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: home})

	got := m.Dispatch(context.Background(), "", Subscriber{ID: "dm", Private: true})
	if !strings.Contains(got, "authorized") || !strings.Contains(got, "/login force") {
		t.Errorf("Dispatch = %q, want the authorized state plus the force hint", got)
	}
}

// TestDispatchStatusAndUsage covers the read-only arguments.
func TestDispatchStatusAndUsage(t *testing.T) {
	m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir()})

	if got := m.Dispatch(context.Background(), "status", dmSub); !strings.Contains(got, "NOT authorized") {
		t.Errorf("status = %q, want the unauthorized state", got)
	}
	if got := m.Dispatch(context.Background(), "help", Subscriber{ID: "dm", Private: true}); got != LoginUsage {
		t.Errorf("help = %q, want LoginUsage", got)
	}
	if got := m.Dispatch(context.Background(), "wat", dmSub); !strings.Contains(got, "Unknown /login argument") {
		t.Errorf("unknown arg = %q, want the usage hint", got)
	}
}

// TestStatusTextNeverLeaksSecrets: an access-token deploy reports only THAT one
// is configured.
func TestStatusTextNeverLeaksSecrets(t *testing.T) {
	m := NewManager(Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir(), HasAccessToken: true,
	})
	got := m.StatusText()
	if !strings.Contains(got, "access token") {
		t.Errorf("StatusText = %q, want it to mention the configured access token", got)
	}
}

// TestDispatchSurvivesADeadRequestContext is the guarantee that makes the whole
// flow usable: the update whose handler ran /login is finished (and its context
// cancelled) long before a human finishes signing in, so the login must NOT be
// tied to it.
func TestDispatchSurvivesADeadRequestContext(t *testing.T) {
	bin := fakeCodex(t, printBanner+persistAuth+"exit 0\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	c := newCollector()
	m.Dispatch(ctx, "", c.sub("chat-1"))
	cancel() // the update's context dies immediately, as it does in production

	c.awaitCode(t)
	if done := c.next(t); !strings.Contains(strings.ToLower(done), "authorized") {
		t.Errorf("final notice = %q, want the login to have completed anyway", done)
	}
}

// TestRequireAuthFalseNeverBlocks: CODEX_REQUIRE_AUTH=false is an explicit
// operator opt-out — they have taken responsibility for credentials this process
// cannot observe (a home mounted later, config.toml). Blocking their runs would
// break a deployment that used to work, so the gate must stay open while /login
// stays offered.
func TestRequireAuthFalseNeverBlocks(t *testing.T) {
	m := NewManager(Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: false, Home: t.TempDir(),
	})

	if !m.Authorized() {
		t.Error("Authorized() = false with CODEX_REQUIRE_AUTH=false")
	}
	if _, blocked := m.BlockedNotice(); blocked {
		t.Error("runs were blocked with CODEX_REQUIRE_AUTH=false")
	}
	status := m.StatusText()
	if !strings.Contains(status, "CODEX_REQUIRE_AUTH=false") {
		t.Errorf("status = %q, want it to explain why nothing is blocked", status)
	}
	// /login must still be usable: without a persisted credential it starts a real
	// sign-in rather than reporting "already authorized".
	if got := m.StatusText(); strings.Contains(got, "already") {
		t.Errorf("status = %q, want it not to claim an existing login", got)
	}
}

// TestSubscriptionLoginDropsInheritedAPIKey: a stray CODEX_API_KEY in the parent
// environment must never reach a subscription sign-in — that is the one path that
// could silently convert a subscription deploy to usage-based API billing.
func TestSubscriptionLoginDropsInheritedAPIKey(t *testing.T) {
	cfg := Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, Home: t.TempDir(),
		Env: []string{"PATH=/usr/bin", "CODEX_API_KEY=sk-live", "CODEX_HOME=/elsewhere"},
	}
	env := cfg.childEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "CODEX_API_KEY=") {
			t.Error("CODEX_API_KEY reached a subscription login")
		}
		if kv == "CODEX_HOME=/elsewhere" {
			t.Error("the inherited CODEX_HOME was not overridden")
		}
	}
	if env[len(env)-1] != "CODEX_HOME="+cfg.Home {
		t.Errorf("CODEX_HOME = %q, want the configured home", env[len(env)-1])
	}
}

// TestNoCodeReachesANonPrivateDestination is the regression for the guard that
// an argument-based check missed: /login status ALSO prints a pending code, so
// the decision has to key off the destination, not the arguments. A group asking
// anything at all must never see the link or the code.
func TestNoCodeReachesANonPrivateDestination(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	// A sign-in is pending, started from a direct message.
	dm := newCollector()
	m.Dispatch(context.Background(), "", Subscriber{ID: "dm", Private: true, Notify: dm.notify})
	dm.awaitCode(t)

	group := Subscriber{ID: "group", Private: false, Notify: func(string) {}}
	for _, args := range []string{"", "status", "force", "again"} {
		t.Run("args="+args, func(t *testing.T) {
			got := m.Dispatch(context.Background(), args, group)
			if strings.Contains(got, deviceCode) {
				t.Errorf("/login %q leaked the one-time code into a group: %q", args, got)
			}
			if strings.Contains(got, deviceURL) {
				t.Errorf("/login %q leaked the verification link into a group: %q", args, got)
			}
		})
	}
}

// TestGroupStatusStillReportsSomethingUseful: withholding the code must not turn
// into silence — a user in a group is told a sign-in is pending and where it went.
func TestGroupStatusStillReportsSomethingUseful(t *testing.T) {
	m := NewManager(Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir(),
	})
	if got := m.Dispatch(context.Background(), "status", Subscriber{ID: "group"}); !strings.Contains(got, "NOT authorized") {
		t.Errorf("group status = %q, want the state (no code is involved when none is pending)", got)
	}
	if got := m.Dispatch(context.Background(), "", Subscriber{ID: "group"}); !strings.Contains(got, "direct message") {
		t.Errorf("group /login = %q, want the direct-message refusal", got)
	}
}

// TestGroupSubscriberNeverJoinsTheBroadcast: a non-private destination must not
// be subscribed either, or the prompt broadcast would hand it the code.
func TestGroupSubscriberNeverJoinsTheBroadcast(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	group := newCollector()
	dm := newCollector()
	m.Dispatch(context.Background(), "", Subscriber{ID: "dm", Private: true, Notify: dm.notify})
	dm.awaitCode(t)
	m.Dispatch(context.Background(), "", Subscriber{ID: "group", Private: false, Notify: group.notify})

	m.Cancel()
	if got := dm.next(t); !strings.Contains(got, "cancelled") {
		t.Fatalf("the private subscriber did not get the outcome: %q", got)
	}
	select {
	case leaked := <-group.ch:
		t.Errorf("a group destination received a login notice: %q", leaked)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCancelWaitsForTheChild: Cancel must not return while the login goroutine is
// still unwinding. Shutdown relies on it to reap the polling child, and a /login
// issued straight after a cancel must start fresh rather than be told "already
// pending" alongside a code that is already dead.
func TestCancelWaitsForTheChild(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("dm"))
	c.awaitCode(t)

	if !m.Cancel() {
		t.Fatal("Cancel reported nothing pending")
	}
	// Immediately after Cancel the manager must already be idle.
	if got := m.StatusText(); strings.Contains(got, "in progress") {
		t.Errorf("StatusText = %q right after Cancel; the run was still marked pending", got)
	}
	if got := m.Dispatch(context.Background(), "", c.sub("dm")); strings.Contains(got, "already pending") {
		t.Errorf("a fresh /login after cancel was refused with a dead code: %q", got)
	}
	m.Cancel()
}

// TestEveryAskerLearnsTheOutcome: a second /login while one is pending re-shows
// the same code — and must also start receiving the verdict. Without the
// subscription, only the chat that happened to start the login would ever learn
// whether it worked.
func TestEveryAskerLearnsTheOutcome(t *testing.T) {
	bin := fakeCodex(t, printBanner+persistAuth+"exit 0\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	first, second := newCollector(), newCollector()
	m.Dispatch(context.Background(), "", first.sub("chat-1"))
	first.awaitCode(t)

	// A different chat asks while the login is pending and is re-shown the code
	// through its own subscription.
	if again := m.Dispatch(context.Background(), "", second.sub("chat-2")); again != "" {
		t.Fatalf("second /login returned %q, want it delivered through the subscriber", again)
	}
	second.awaitCode(t)

	for name, c := range map[string]*collector{"starter": first, "joiner": second} {
		if got := c.next(t); !strings.Contains(strings.ToLower(got), "authorized") {
			t.Errorf("%s got %q as the final notice, want the success confirmation", name, got)
		}
	}
}

// TestTheSameChatAskingTwiceIsNotToldTwice: subscriptions are keyed by
// destination, so a user tapping /login twice does not double every later notice.
func TestTheSameChatAskingTwiceIsNotToldTwice(t *testing.T) {
	bin := fakeCodex(t, printBanner+persistAuth+"exit 0\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("chat-1"))
	c.awaitCode(t)
	// Asking again re-shows the code to the asker — that is what asking is for —
	// but must not double every notice that FOLLOWS.
	m.Dispatch(context.Background(), "", c.sub("chat-1"))
	c.awaitCode(t)

	if got := c.next(t); !strings.Contains(strings.ToLower(got), "authorized") {
		t.Fatalf("final notice = %q, want the success confirmation", got)
	}
	select {
	case extra := <-c.ch:
		t.Errorf("the same chat was told twice; extra notice: %q", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestCleanExitWithoutACredentialIsNotSuccess: what authorizes the deployment is
// the credential on disk, which is what the run gate reads. Announcing success on
// the CLI's exit code alone would tell the user they are signed in and then
// refuse their very next message.
func TestCleanExitWithoutACredentialIsNotSuccess(t *testing.T) {
	home := t.TempDir()
	bin := fakeCodex(t, printBanner+"exit 0\n") // exits clean, writes nothing
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        home,
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("dm"))
	c.awaitCode(t)

	got := c.next(t)
	if strings.Contains(strings.ToLower(got), "codex is authorized") {
		t.Errorf("reported success with no persisted credential: %q", got)
	}
	if !strings.Contains(got, home) {
		t.Errorf("final notice = %q, want it to name the directory that stayed empty", got)
	}
	if _, blocked := m.BlockedNotice(); !blocked {
		t.Error("runs were unblocked by a login that persisted nothing")
	}
}

// TestAckPrecedesTheCode pins the message ORDER: the acknowledgement must not
// surface underneath the code it promises.
func TestAckPrecedesTheCode(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	c := newCollector()
	if reply := m.Dispatch(context.Background(), "", c.sub("dm")); reply != "" {
		t.Fatalf("Dispatch returned %q, want it delivered through the subscriber", reply)
	}
	// Returning the acknowledgement for the caller to send separately raced the
	// broadcast: a CLI that printed its banner quickly got the code out first, and
	// the user read "the code arrives in a moment" underneath the code.
	if got := c.next(t); !strings.Contains(got, "Starting") {
		t.Errorf("first notice = %q, want the acknowledgement before the code", got)
	}
	if got := c.next(t); !strings.Contains(got, deviceCode) {
		t.Errorf("second notice = %q, want the code", got)
	}
}

// TestDispatchStillReturnsAReplyWithoutASubscriber: a caller that cannot receive
// notices (no Notify) must still get the text to send itself.
func TestDispatchStillReturnsAReplyWithoutASubscriber(t *testing.T) {
	m := NewManager(Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir(),
		Bin: filepath.Join(t.TempDir(), "no-such-codex"),
	})
	t.Cleanup(func() { m.Cancel() })

	if got := m.Dispatch(context.Background(), "", Subscriber{ID: "dm", Private: true}); got == "" {
		t.Error("Dispatch swallowed the reply for a subscriber that cannot receive it")
	}
}

// TestGroupStatusHidesTheHostPath: the authorized branch names CODEX_HOME, which
// is deployment detail a group chat has no business with. /login status reaches
// that branch from anywhere, so the path is private-only too.
func TestGroupStatusHidesTheHostPath(t *testing.T) {
	home := t.TempDir()
	writeAuthFile(t, home)
	m := NewManager(Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: home,
	})

	group := m.Dispatch(context.Background(), "status", Subscriber{ID: "group"})
	if strings.Contains(group, home) {
		t.Errorf("group status leaked the host path: %q", group)
	}
	if !strings.Contains(group, "authorized") {
		t.Errorf("group status = %q, want it to still report the state", group)
	}
	if dm := m.Dispatch(context.Background(), "status", dmSub); !strings.Contains(dm, home) {
		t.Errorf("direct-message status = %q, want the path an operator needs", dm)
	}
}

// TestCancelIsPrivateOnly: cancel is the one state-MUTATING argument, so it obeys
// the same rule as starting. Otherwise a conversation could abort a sign-in
// running in someone else's direct message — and the confirmation would itself
// reveal that one is under way.
func TestCancelIsPrivateOnly(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("dm"))
	c.awaitCode(t)

	got := m.Dispatch(context.Background(), "cancel", Subscriber{ID: "group"})
	if got != cancelPrivateOnlyText {
		t.Errorf("group cancel = %q, want the cancel-specific refusal", got)
	}
	// The refusal must explain ITSELF: a cancel reply carries no code, so borrowing
	// the start refusal's reasoning would simply be untrue.
	if strings.Contains(got, "one-time code") {
		t.Errorf("cancel refusal = %q, want it not to claim a code is at stake", got)
	}
	if !strings.Contains(m.StatusText(), "in progress") {
		t.Error("a group aborted a sign-in owned by a direct message")
	}

	if reply := m.Dispatch(context.Background(), "cancel", dmSub); !strings.Contains(reply, "Cancelled") {
		t.Errorf("direct-message cancel = %q, want it to work", reply)
	}
}

// TestCancelLeavesNoPendingStateEvenOnTimeout is the state-machine regression.
// The drain is reachable in normal operation — the login goroutine may be inside
// a notice delivery, bounded by NotifyTimeout, which is longer than cancelDrain.
// When that happened, Cancel cleared the cancel func but left the attempt marked
// running: the next /login re-showed the code it had just killed, and the next
// /login cancel answered "nothing is pending". The attempt is now detached before
// any waiting, so a timeout leaves nothing behind.
func TestCancelLeavesNoPendingStateEvenOnTimeout(t *testing.T) {
	bin := fakeCodex(t, printBanner+"sleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	// A subscriber that takes the acknowledgement and then wedges on the prompt,
	// standing in for a transport that drops into a 429 back-off. The login
	// goroutine is thus stuck inside a delivery — which is what makes the drain
	// reachable, since a delivery is bounded by NotifyTimeout, not by cancelDrain.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var delivered atomic.Int64
	stuck := Subscriber{ID: "dm", Private: true, Notify: func(string) {
		if delivered.Add(1) > 1 {
			<-release
		}
	}}

	m.Dispatch(context.Background(), "", stuck)
	waitFor(t, func() bool { return delivered.Load() > 1 })

	if !m.Cancel() {
		t.Fatal("Cancel reported nothing pending")
	}
	if got := m.StatusText(); strings.Contains(got, "in progress") {
		t.Errorf("StatusText = %q after Cancel; the attempt was left marked pending", got)
	}
	if m.Cancel() {
		t.Error("a second Cancel found something pending")
	}
	// The decisive symptom: a fresh /login must not be answered with the dead code.
	if got := m.Dispatch(context.Background(), "", dmSub); strings.Contains(got, "already pending") {
		t.Errorf("a fresh /login after cancel = %q, want a new attempt", got)
	}
}

// TestOutcomeGoesToTheAttemptThatProducedIt: the verdict is delivered to a
// snapshot of THIS attempt's subscribers. Reading the live list instead let a
// successor's chat receive the predecessor's failure — seconds after its own
// "Starting the sign-in" — while the chat that actually ran it heard nothing.
func TestOutcomeGoesToTheAttemptThatProducedIt(t *testing.T) {
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         fakeCodex(t, printBanner+"sleep 30\n"),
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	first, second := newCollector(), newCollector()
	m.Dispatch(context.Background(), "", first.sub("chat-a"))
	first.awaitCode(t)

	// Cancel detaches attempt A; B starts while A is still unwinding.
	m.Cancel()
	m.Dispatch(context.Background(), "", second.sub("chat-b"))

	// A's cancellation reaches A, and never B.
	found := false
	for range 3 {
		if strings.Contains(strings.ToLower(first.next(t)), "cancelled") {
			found = true
			break
		}
	}
	if !found {
		t.Error("the cancelled attempt's own chat never heard its outcome")
	}
	for _, text := range drain(second) {
		if strings.Contains(strings.ToLower(text), "cancelled") {
			t.Errorf("the successor's chat received the predecessor's verdict: %q", text)
		}
	}
}

// drain returns every notice delivered so far, without blocking.
func drain(c *collector) []string {
	var out []string
	for {
		select {
		case text := <-c.ch:
			out = append(out, text)
		default:
			return out
		}
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// TestSilenceWithoutACodeIsReported guards against the package's own failure
// mode returning by the back door. If the parser never pairs a URL with a code —
// a changed banner, a code printed with an unexpected prefix — the user would
// read "the link and one-time code arrive in a moment" and then hear nothing for
// DeviceCodeTTL. That is "it just hangs" with friendlier wording, and it is the
// symptom this package exists to remove.
func TestSilenceWithoutACodeIsReported(t *testing.T) {
	restore := promptWarnAfter
	promptWarnAfter = 50 * time.Millisecond
	t.Cleanup(func() { promptWarnAfter = restore })

	// A CLI that prints something unparseable and then polls, as a drifted banner
	// would.
	bin := fakeCodex(t, "echo 'signing you in, please hold'\nsleep 30\n")
	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         bin,
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("dm"))
	if ack := c.next(t); !strings.Contains(ack, "Starting") {
		t.Fatalf("first notice = %q, want the acknowledgement", ack)
	}

	got := c.next(t)
	if !strings.Contains(got, "hasn't printed a sign-in code") {
		t.Errorf("notice = %q, want the no-code warning", got)
	}
	if !strings.Contains(got, "/login cancel") {
		t.Errorf("notice = %q, want it to name the way out", got)
	}
}

// TestNoSilenceWarningOnceTheCodeArrives: the guard must stay quiet on the happy
// path, or every login would carry a spurious "no code yet".
func TestNoSilenceWarningOnceTheCodeArrives(t *testing.T) {
	restore := promptWarnAfter
	promptWarnAfter = 50 * time.Millisecond
	t.Cleanup(func() { promptWarnAfter = restore })

	m := NewManager(Config{
		Backend:     BackendCodex,
		AuthMode:    AuthSubscription,
		RequireAuth: true,
		Bin:         fakeCodex(t, printBanner+"sleep 30\n"),
		Home:        t.TempDir(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	c := newCollector()
	m.Dispatch(context.Background(), "", c.sub("dm"))
	c.awaitCode(t)

	select {
	case extra := <-c.ch:
		t.Errorf("a warning fired after the code arrived: %q", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBlockedNoticePointsAtADirectMessage: the run gate cannot see the
// destination, so the notice it sends must be correct in a group too — telling a
// user to send /login where it will be refused earns them two refusals in a row,
// possibly in the only chat they use with the bot.
func TestBlockedNoticePointsAtADirectMessage(t *testing.T) {
	m := NewManager(Config{
		Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir(),
	})
	notice, blocked := m.BlockedNotice()
	if !blocked {
		t.Fatal("BlockedNotice reported no block while unauthorized")
	}
	if !strings.Contains(notice, "direct message") {
		t.Errorf("notice = %q, want it to name where /login is accepted", notice)
	}
}
