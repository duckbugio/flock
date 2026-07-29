//nolint:testpackage // intentionally whitebox to test the device-login parser and manager internals
package codexauth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func (c *collector) notify(text string) { c.ch <- text }

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
	if got := m.Dispatch(context.Background(), "", nil); got != NoLoginNeededText {
		t.Errorf("nil Manager Dispatch = %q, want NoLoginNeededText", got)
	}
}

// TestDispatchNotApplicable: /login on a Claude or billing deployment explains
// itself instead of running anything.
func TestDispatchNotApplicable(t *testing.T) {
	claude := NewManager(Config{Backend: "claude"})
	if got := claude.Dispatch(context.Background(), "", nil); !strings.Contains(got, "claude") {
		t.Errorf("Dispatch = %q, want it to name the configured backend", got)
	}
	billing := NewManager(Config{Backend: BackendCodex, AuthMode: AuthBilling})
	if got := billing.Dispatch(context.Background(), "", nil); !strings.Contains(got, "CODEX_API_KEY") {
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
	if reply := m.Dispatch(context.Background(), "", c.notify); !strings.Contains(reply, "Starting") {
		t.Errorf("immediate reply = %q, want an acknowledgement that the sign-in started", reply)
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
	m.Dispatch(context.Background(), "", c.notify)
	if prompt := c.next(t); !strings.Contains(prompt, "XER9-NWCA2") {
		t.Fatalf("first prompt %q is missing the code", prompt)
	}

	again := m.Dispatch(context.Background(), "", c.notify)
	if !strings.Contains(again, "XER9-NWCA2") {
		t.Errorf("second /login = %q, want the pending code re-shown", again)
	}
	if !strings.Contains(again, "already pending") {
		t.Errorf("second /login = %q, want it to say a login is already pending", again)
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

	if got := m.Dispatch(context.Background(), "cancel", nil); !strings.Contains(got, "No Codex login is pending") {
		t.Errorf("cancel with nothing pending = %q", got)
	}

	c := newCollector()
	m.Dispatch(context.Background(), "", c.notify)
	c.next(t) // the prompt

	if got := m.Dispatch(context.Background(), "cancel", c.notify); !strings.Contains(got, "Cancelled") {
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

	got := m.Dispatch(context.Background(), "", nil)
	if !strings.Contains(got, "authorized") || !strings.Contains(got, "/login force") {
		t.Errorf("Dispatch = %q, want the authorized state plus the force hint", got)
	}
}

// TestDispatchStatusAndUsage covers the read-only arguments.
func TestDispatchStatusAndUsage(t *testing.T) {
	m := NewManager(Config{Backend: BackendCodex, AuthMode: AuthSubscription, RequireAuth: true, Home: t.TempDir()})

	if got := m.Dispatch(context.Background(), "status", nil); !strings.Contains(got, "NOT authorized") {
		t.Errorf("status = %q, want the unauthorized state", got)
	}
	if got := m.Dispatch(context.Background(), "help", nil); got != LoginUsage {
		t.Errorf("help = %q, want LoginUsage", got)
	}
	if got := m.Dispatch(context.Background(), "wat", nil); !strings.Contains(got, "Unknown /login argument") {
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
	bin := fakeCodex(t, printBanner+"sleep 0.3\nexit 0\n")
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
	m.Dispatch(ctx, "", c.notify)
	cancel() // the update's context dies immediately, as it does in production

	if prompt := c.next(t); !strings.Contains(prompt, "XER9-NWCA2") {
		t.Errorf("prompt %q lost after the request context was cancelled", prompt)
	}
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
