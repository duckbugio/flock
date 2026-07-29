package codexauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Codex auth modes, mirroring internal/config's CODEX_AUTH_MODE values. Only the
// subscription mode has an interactive login: billing authenticates with an API
// key from the environment and has nothing for a user to confirm in a browser.
const (
	AuthSubscription = "subscription"
	AuthBilling      = "billing"
)

// BackendCodex is the AI_BACKEND value this package applies to. Any other
// backend (Claude, an OpenAI-compatible endpoint) authenticates from env alone,
// so /login is a no-op there.
const BackendCodex = "codex"

// Config describes the deployment's Codex auth setup. It is built once at
// startup (see airunner.CodexAuthConfig) and read for every /login.
type Config struct {
	// Backend is the resolved AI_BACKEND name.
	Backend string
	// AuthMode is the resolved CODEX_AUTH_MODE ("subscription" or "billing").
	AuthMode string
	// Bin is the Codex CLI path (CODEX_BIN); empty means "codex" on PATH.
	Bin string
	// Home is CODEX_HOME — the directory holding the persisted auth.json that a
	// successful login writes and every later run reads.
	Home string
	// Env is the child environment for the Codex CLI, already stripped of
	// CODEX_API_KEY in subscription mode. Empty inherits the parent's.
	Env []string
	// HasAccessToken records that CODEX_ACCESS_TOKEN is configured, which
	// authorizes Codex without any interactive login.
	HasAccessToken bool
	// RequireAuth mirrors CODEX_REQUIRE_AUTH. When false the operator has
	// explicitly declared that Codex is authorized by means this process cannot
	// see (a home mounted later, credentials in config.toml), so runs are never
	// blocked — /login stays available, it just is not demanded.
	RequireAuth bool
	// Timeout bounds one login attempt. Zero means DeviceCodeTTL plus a minute of
	// slack, so an abandoned login cannot outlive the code it waits on.
	Timeout time.Duration
	Logger  *slog.Logger
}

// bin returns the Codex CLI to execute.
func (c Config) bin() string {
	if b := strings.TrimSpace(c.Bin); b != "" {
		return b
	}
	return "codex"
}

// childEnv returns the environment for the login child.
//
// CODEX_HOME is forced to the configured value so the login writes auth.json
// exactly where the runner later looks for it — a login into a different home is
// the subtlest way for "logged in" and "still unauthorized" to be true at once.
//
// CODEX_API_KEY is dropped in subscription mode. Env normally arrives already
// stripped (airunner.CodexEnv), but this is the one place a stray inherited key
// could turn a subscription sign-in into API-billing auth, so the invariant is
// enforced here too rather than assumed.
func (c Config) childEnv() []string {
	env := c.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	dropAPIKey := strings.EqualFold(strings.TrimSpace(c.AuthMode), AuthSubscription)
	home := strings.TrimSpace(c.Home)

	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if home != "" && strings.HasPrefix(kv, "CODEX_HOME=") {
			continue
		}
		if dropAPIKey && strings.HasPrefix(kv, "CODEX_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	if home == "" {
		return out
	}
	return append(out, "CODEX_HOME="+home)
}

// authFile is the persisted login file a successful device login writes.
func (c Config) authFile() string {
	home := strings.TrimSpace(c.Home)
	if home == "" {
		return ""
	}
	return filepath.Join(home, "auth.json")
}

// timeout bounds a single login attempt.
func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DeviceCodeTTL + time.Minute
}

// logger never returns nil.
func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Manager owns the at-most-one in-flight device login for the process and
// answers the /login command. Codex auth is process-wide state (a single
// auth.json under CODEX_HOME), so the login is deliberately NOT per chat: a
// second /login while one is pending re-shows the SAME code rather than starting
// a competing flow that would invalidate it.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	last    Prompt
	hasLast bool
}

// NewManager returns a Manager for cfg.
func NewManager(cfg Config) *Manager { return &Manager{cfg: cfg} }

// Applicable reports whether an interactive login means anything on this
// deployment: only the Codex backend in subscription mode has one.
func (m *Manager) Applicable() bool {
	if m == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(m.cfg.Backend), BackendCodex) &&
		strings.EqualFold(strings.TrimSpace(m.cfg.AuthMode), AuthSubscription)
}

// Authorized reports whether Codex can run right now: a configured access token,
// or a persisted auth.json under CODEX_HOME. It re-checks the file on every call
// rather than caching a startup verdict, so the very first run after a
// successful /login is allowed through without a restart.
//
// CODEX_REQUIRE_AUTH=false opts out of the check entirely, exactly as it does at
// startup: an operator who disabled it has taken responsibility for credentials
// this process cannot observe, and must not have their runs blocked.
func (m *Manager) Authorized() bool {
	if !m.Applicable() || !m.cfg.RequireAuth {
		return true
	}
	return m.credentialsPresent()
}

// credentialsPresent reports whether Codex has something to authenticate WITH,
// independent of whether the deployment demands it. It is the honest answer
// /login reasons about; Authorized is the policy answer runs are gated on.
func (m *Manager) credentialsPresent() bool {
	if m == nil {
		return false
	}
	if m.cfg.HasAccessToken {
		return true
	}
	f := m.cfg.authFile()
	if f == "" {
		return false
	}
	_, err := os.Stat(f)
	return err == nil
}

// NeedsLogin is the inverse of Authorized, for callers that read better that way.
func (m *Manager) NeedsLogin() bool { return !m.Authorized() }

// BlockedNotice returns the reply to send instead of starting a run while Codex
// is unauthorized, and whether the run must be blocked at all. Blocking here —
// rather than letting the run fail deep inside the CLI — is what makes the
// unauthorized state recoverable: the user is told the one command that fixes it.
func (m *Manager) BlockedNotice() (string, bool) {
	if m == nil || m.Authorized() {
		return "", false
	}
	return "Codex is not authorized yet, so I can't run anything.\n\n" +
		"Send /login and I'll walk you through the one-time browser sign-in.", true
}

// NoLoginNeededText is the reply when this deployment has no interactive login
// at all — including when no Manager was wired up.
const NoLoginNeededText = "No interactive login is needed on this deployment: " +
	"the AI provider authenticates from its configured credentials."

// StatusText describes the current auth state without ever revealing a secret.
func (m *Manager) StatusText() string {
	if !m.Applicable() {
		return m.notApplicableText()
	}
	m.mu.Lock()
	running, last, hasLast := m.running, m.last, m.hasLast
	m.mu.Unlock()

	switch {
	case running && hasLast:
		return "Codex device login is in progress — finish it in the browser:\n\n" + promptText(last)
	case running:
		return "Codex device login is starting; the link and code arrive in a moment."
	case m.cfg.HasAccessToken:
		return "Codex is authorized with a configured access token. No browser login needed."
	case m.credentialsPresent():
		return "Codex is authorized (persisted login in " + m.cfg.Home + ")."
	case !m.cfg.RequireAuth:
		return "No persisted Codex login found, but CODEX_REQUIRE_AUTH=false, so runs are not blocked. " +
			"Send /login if Codex reports it is unauthenticated."
	default:
		return "Codex is NOT authorized. Send /login to sign in with your ChatGPT account."
	}
}

// notApplicableText explains why /login does nothing on this deployment. It is
// nil-safe: an adapter wired without a Manager still gets a sensible reply
// instead of a panic.
func (m *Manager) notApplicableText() string {
	if m == nil {
		return NoLoginNeededText
	}
	backend := strings.TrimSpace(m.cfg.Backend)
	if backend == "" {
		backend = "the configured provider"
	}
	if strings.EqualFold(backend, BackendCodex) {
		return "Codex runs in API-billing mode here, which authenticates from CODEX_API_KEY. " +
			"There is no browser login to complete."
	}
	return "No interactive login is needed: this deployment runs " + backend +
		", which authenticates from its configured credentials."
}

// LoginUsage is the /login help.
const LoginUsage = "Usage:\n" +
	"/login — start the Codex browser sign-in (or re-show the pending code)\n" +
	"/login status — show the current authorization state\n" +
	"/login cancel — abort a pending sign-in\n" +
	"/login force — sign in again even if already authorized"

// Dispatch serves the /login command and returns the IMMEDIATE reply.
//
// The device flow outlives the command: after the immediate reply, the pending
// login keeps running in the background and reports the verification link, the
// one-time code, and the final outcome through notify. notify may therefore be
// called minutes later and from another goroutine — the caller must make it safe
// to use then (its own context, not the update's).
//
// base only seeds the login's values; its cancellation does NOT abort the login
// (the update's context is dead long before a user finishes in a browser).
// /login cancel, the timeout, and process shutdown are the ways it ends.
func (m *Manager) Dispatch(base context.Context, args string, notify func(string)) string {
	if !m.Applicable() {
		return m.notApplicableText()
	}
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "":
		return m.start(base, false, notify)
	case "force", "again", "relogin":
		return m.start(base, true, notify)
	case "status":
		return m.StatusText()
	case "cancel", "abort", "stop":
		if m.Cancel() {
			return "Cancelled the pending Codex login."
		}
		return "No Codex login is pending."
	case "help":
		return LoginUsage
	default:
		return "Unknown /login argument.\n\n" + LoginUsage
	}
}

// start launches a device login unless one is already pending (in which case the
// pending code is re-shown, never replaced) or Codex is already authorized and
// force was not asked for.
func (m *Manager) start(base context.Context, force bool, notify func(string)) string {
	m.mu.Lock()
	if m.running {
		last, hasLast := m.last, m.hasLast
		m.mu.Unlock()
		if hasLast {
			return "A Codex login is already pending — finish this one:\n\n" + promptText(last)
		}
		return "A Codex login is already starting; the link and code arrive in a moment."
	}
	// credentialsPresent, not Authorized: with CODEX_REQUIRE_AUTH=false the policy
	// answer is always "authorized", which would make /login unusable on exactly
	// the deployments most likely to need it.
	if !force && m.credentialsPresent() {
		m.mu.Unlock()
		return m.StatusText() + "\n\nSend /login force to sign in again."
	}

	// context.WithoutCancel: the login must survive the update whose handler
	// started it. The timeout is the real bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(base), m.cfg.timeout())
	m.running = true
	m.cancel = cancel
	m.hasLast = false
	m.last = Prompt{}
	m.mu.Unlock()

	go m.run(ctx, cancel, notify)
	return "Starting the Codex sign-in — the link and one-time code arrive in a moment."
}

// run drives one login attempt and reports its result through notify.
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, notify func(string)) {
	defer cancel()
	log := m.cfg.logger()

	err := Login(ctx, m.cfg, func(p Prompt) {
		m.mu.Lock()
		m.last, m.hasLast = p, true
		m.mu.Unlock()
		log.Info("codex device login prompt issued", "url", p.URL, "expires_in", p.ExpiresIn)
		if notify != nil {
			notify(promptText(p))
		}
	})

	m.mu.Lock()
	m.running = false
	m.cancel = nil
	m.mu.Unlock()

	if notify == nil {
		return
	}
	if err == nil {
		log.Info("codex device login succeeded", "codex_home", m.cfg.Home)
		notify("Codex is authorized. Send your next message and the team gets to work.")
		return
	}
	log.Warn("codex device login failed", "error", err)
	notify(failureText(err))
}

// Cancel aborts a pending login, reporting whether there was one.
func (m *Manager) Cancel() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.cancel == nil {
		return false
	}
	m.cancel()
	m.cancel = nil
	return true
}

// promptText renders the user-facing sign-in instructions. The code is the whole
// point of the message, so it goes on its own line, unadorned, ready to copy.
func promptText(p Prompt) string {
	var b strings.Builder
	_, _ = b.WriteString("Codex sign-in — two steps:\n\n1. Open this link and sign in to your ChatGPT account:\n")
	_, _ = b.WriteString(p.URL)
	_, _ = b.WriteString("\n\n2. Enter this one-time code there:\n")
	_, _ = b.WriteString(p.Code)
	if p.ExpiresIn > 0 {
		_, _ = fmt.Fprintf(&b, "\n\nThe code expires in %s.", humanDuration(p.ExpiresIn))
	}
	_, _ = b.WriteString("\n\nI'll confirm here as soon as it goes through. /login cancel aborts it.")
	return b.String()
}

// failureText turns a login error into an actionable reply, keeping the CLI's
// own words for the cases we cannot classify.
func failureText(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "The Codex sign-in timed out — the one-time code expired before it was confirmed. " +
			"Send /login to get a fresh code."
	case errors.Is(err, context.Canceled):
		return "The Codex sign-in was cancelled."
	case errors.Is(err, ErrDeviceAuthUnsupported):
		return "Codex sign-in is unavailable: " + err.Error()
	default:
		return "Codex sign-in failed: " + err.Error() + "\n\nSend /login to try again."
	}
}

// humanDuration renders a code lifetime the way a person would say it.
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
}
