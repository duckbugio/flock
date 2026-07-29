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

	// notified is the ONE "already told them" registry for the refusal notice — see
	// NoticeFor. It has its own lock so a notice never contends with a login.
	notified onceNotifier

	mu sync.Mutex
	// cur is the attempt in flight, nil when idle. Holding the whole attempt behind
	// ONE pointer is what keeps the state machine honest: "is a login pending",
	// "whose code is this", and "who is waiting to hear the outcome" are answers
	// about one attempt, and swapping the pointer under the lock moves all of them
	// at once. Kept as separate fields they could — and did — disagree: a Cancel
	// that timed out cleared the cancel func but left the attempt marked running,
	// so the next /login re-showed a code that had just been killed.
	cur *session
}

// session is one device-login attempt. The run goroutine owns its lifetime; every
// field is guarded by Manager.mu.
type session struct {
	// cancelled records that this attempt was ended deliberately, so its verdict is
	// not also announced: Cancel already told the chat, and "The Codex sign-in was
	// cancelled" arriving after "Cancelled the pending Codex login" is the same news
	// twice.
	cancelled bool
	cancel    context.CancelFunc
	done      chan struct{} // closed when this attempt's goroutine has returned
	// prompt is the verification link and one-time code, once issued.
	prompt    Prompt
	hasPrompt bool
	// subs are the destinations waiting on THIS attempt's outcome.
	subs []Subscriber
}

// NewManager returns a Manager for cfg.
func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg, notified: onceNotifier{told: map[string]bool{}}}
}

// onceNotifier tells each destination something at most once, until Clear.
type onceNotifier struct {
	mu   sync.Mutex
	told map[string]bool
}

// Should reports whether dest still needs to be told, marking it as told.
func (n *onceNotifier) Should(dest string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.told == nil {
		n.told = map[string]bool{}
	}
	if n.told[dest] {
		return false
	}
	n.told[dest] = true
	return true
}

// Clear forgets every destination, so the next occurrence is announced again.
func (n *onceNotifier) Clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	clear(n.told)
}

// Applicable reports whether an interactive login means anything on this
// deployment: only the Codex backend in subscription mode has one.
func (m *Manager) Applicable() bool {
	if m == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(m.cfg.Backend), BackendCodex) &&
		strings.EqualFold(strings.TrimSpace(m.cfg.AuthMode), AuthSubscription)
}

// LoginAdvertised reports whether /login should be OFFERED — in a command menu
// or a help text. It is narrower than Applicable: a deployment carrying
// CODEX_ACCESS_TOKEN is already authorized without a browser, so listing the
// command would lead the user to a reply that says it is not needed, which is the
// exact reason the other backends are excluded.
//
// Applicable stays wider on purpose: /login force is still a legitimate way to
// replace a workspace token with a personal subscription, so the command keeps
// working where it is simply not advertised.
func (m *Manager) LoginAdvertised() bool {
	if m == nil {
		return false
	}
	return m.Applicable() && !m.cfg.HasAccessToken
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
	return m.CredentialsPresent()
}

// CredentialsPresent reports whether Codex has something to authenticate WITH: a
// configured access token, or a persisted auth.json under CODEX_HOME.
//
// This is the FACTUAL answer; Authorized is the POLICY answer runs are gated on,
// and the two deliberately differ when CODEX_REQUIRE_AUTH=false — that deploy has
// no credential and is not blocked. Startup logs both, because "authorized=true"
// alone would reassure an operator in exactly the configuration where the first
// run is about to fail inside the CLI.
//
// A present auth.json is not proof the credential still WORKS: an expired or
// corrupt token reads as present. Re-validating would mean spawning
// `codex login status` on every message, which is far too expensive for the
// message path; /login force re-authenticates when a stale credential shows up.
func (m *Manager) CredentialsPresent() bool {
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

// Blocked reports whether runs must be refused right now. It has no side effects,
// so a caller that only needs to know — the follow-up sweeper and the cron
// scheduler, which skip their whole pass rather than consume work — can ask
// freely.
func (m *Manager) Blocked() bool {
	if m == nil {
		return false
	}
	return !m.Authorized()
}

// NoticeFor returns the explanation to send to dest, or "" when dest has already
// been told since the last time the block cleared.
//
// The "told once" registry lives HERE, with the state it describes, because the
// refusal reaches a chat from two layers: an adapter answering a user's message,
// and core/chat refusing a background submission. A registry per layer keeps each
// layer's own repeats down but lets a chat be told twice — once by the poller's
// refusal, once by its own next message — and neither layer's tests can see it,
// because each exercises only itself.
func (m *Manager) NoticeFor(dest string) string {
	if m == nil {
		return ""
	}
	if !m.Blocked() {
		// Authorization is deployment-wide, so its return clears every destination:
		// a chat that stayed quiet through the recovery still hears about the NEXT
		// lapse.
		m.notified.Clear()
		return ""
	}
	if !m.notified.Should(dest) {
		return ""
	}
	// "in a direct message", because that is where the command will be accepted:
	// the caller cannot see the destination, and sending the user to /login in a
	// group would only earn them a second refusal — possibly in the only chat they
	// use with the bot.
	return "Codex is not authorized yet, so I can't run anything.\n\n" +
		"Send /login in a direct message with me and I'll walk you through the one-time browser sign-in."
}

// NoLoginNeededText is the reply when this deployment has no interactive login
// at all — including when no Manager was wired up.
const NoLoginNeededText = "No interactive login is needed on this deployment: " +
	"the AI provider authenticates from its configured credentials."

// StatusText describes the current auth state for a PRIVATE destination. It
// never reveals a stored secret, but it does re-show a pending sign-in's
// one-time code, which is why the destination matters — see statusText.
func (m *Manager) StatusText() string { return m.statusText(true) }

// statusText describes the current auth state. When the destination is not
// private the pending code is withheld: /login status is otherwise a way to
// print it into a group, straight past the guard on starting a sign-in there.
func (m *Manager) statusText(private bool) string {
	if !m.Applicable() {
		return m.notApplicableText()
	}
	m.mu.Lock()
	cur := m.cur
	var (
		prompt    Prompt
		hasPrompt bool
	)
	if cur != nil {
		prompt, hasPrompt = cur.prompt, cur.hasPrompt
	}
	m.mu.Unlock()

	switch {
	case cur != nil && hasPrompt && !private:
		return pendingElsewhereText
	case cur != nil && hasPrompt:
		return "Codex device login is in progress — finish it in the browser:\n\n" + promptText(prompt)
	case cur != nil && !private:
		// Not "the code arrives in a moment": a non-private destination never
		// subscribes, so nothing would ever arrive there.
		return pendingElsewhereText
	case cur != nil:
		return "Codex device login is starting; the link and code arrive in a moment."
	case m.cfg.HasAccessToken:
		return "Codex is authorized with a configured access token. No browser login needed."
	case m.CredentialsPresent() && private:
		return "Codex is authorized (persisted login in " + m.cfg.Home + ")."
	case m.CredentialsPresent():
		// The host path is deployment detail; a group chat has no business with it.
		return "Codex is authorized."
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

// LoginPrivateOnlyText refuses to START a sign-in outside a 1:1 chat.
const LoginPrivateOnlyText = "Send /login in a direct message with me, not here.\n\n" +
	"The reply carries a one-time code that authorizes an account for the whole bot, " +
	"and everyone in this chat would see it."

// cancelPrivateOnlyText refuses to abort a sign-in from outside a 1:1 chat. It is
// separate from LoginPrivateOnlyText because the reason differs: a cancel reply
// carries no code, so explaining the refusal in terms of one would be simply
// untrue. What is at stake is control — the sign-in belongs to the direct message
// running it, and its confirmation would reveal that one is under way.
const cancelPrivateOnlyText = "Send /login cancel in a direct message with me, not here.\n\n" +
	"A sign-in is controlled from the direct message it runs in."

// pendingElsewhereText reports a pending sign-in without reprinting its code. It
// is deliberately neutral about WHOSE direct message: the code is re-shown to any
// private destination that asks, not only the one that started the login, and
// telling the user otherwise would misdescribe the very thing this message is
// about. (That any allow-listed user can finish a started sign-in with their own
// account is a documented property — one Codex identity serves the deployment.)
const pendingElsewhereText = "A Codex sign-in is in progress. Its link and one-time code go only to direct " +
	"messages — send /login in a direct message with me to see them again."

// LoginUsage is the /login help.
const LoginUsage = "Usage:\n" +
	"/login — start the Codex browser sign-in (or re-show the pending code)\n" +
	"/login status — show the current authorization state\n" +
	"/login cancel — abort a pending sign-in\n" +
	"/login force — sign in again even if already authorized"

// NotifyTimeout bounds ONE background notice delivery. Both adapters use it for
// their Subscriber.Notify: a pending login reports minutes after the update that
// started it is gone, so each notice needs a fresh, bounded context of its own.
const NotifyTimeout = 30 * time.Second

// Subscriber is where a /login reply and a pending login's background notices
// are delivered.
//
// Private is the security-critical field: the one-time code authorizes an
// account for the WHOLE bot, so whoever acts on it first binds their ChatGPT
// account to it and every later run (and its history) goes through that account.
// A code may therefore only ever reach a 1:1 destination. The judgement lives
// HERE rather than in each adapter because it cannot be derived from the command
// arguments: /login status also prints the pending code, so an argument-based
// guard silently misses it. Manager redacts instead, on every path that could
// carry a code.
//
// ID identifies the destination so the same one asking twice is collapsed to a
// single subscription rather than being told everything twice. Notify does the
// delivery and may be called minutes later, from another goroutine, so it must
// build its own context rather than close over the update's.
type Subscriber struct {
	ID      string
	Private bool
	Notify  func(string)
}

// deliver sends text when the subscriber can receive it.
func (s Subscriber) deliver(text string) {
	if s.Notify != nil {
		s.Notify(text)
	}
}

// normalizeArgs canonicalizes a /login argument for comparison.
func normalizeArgs(args string) string { return strings.ToLower(strings.TrimSpace(args)) }

// Dispatch serves the /login command and returns the immediate reply for the
// caller to send — or "" when it has already been delivered through sub, which is
// how the reply is kept ahead of the notices that follow it.
//
// The device flow outlives the command: after the immediate reply, the pending
// login keeps running in the background and reports the verification link, the
// one-time code, and the final outcome to every subscriber.
//
// Subscribing is per Dispatch call, so a SECOND /login (from another chat, or the
// same one) both re-shows the pending code and starts receiving the outcome —
// without it, only the chat that happened to start the login would ever learn
// whether it succeeded.
//
// base only seeds the login's values; its cancellation does NOT abort the login
// (the update's context is dead long before a user finishes in a browser).
// /login cancel, the timeout, and process shutdown are the ways it ends.
func (m *Manager) Dispatch(base context.Context, args string, sub Subscriber) string {
	if !m.Applicable() {
		return m.notApplicableText()
	}
	switch normalizeArgs(args) {
	case "":
		return m.start(base, false, sub)
	case "force", "again", "relogin":
		return m.start(base, true, sub)
	case "status":
		return m.statusText(sub.Private)
	case "cancel", "abort", "stop":
		// The only state-MUTATING branch, so it obeys the same rule as start: a
		// conversation could otherwise abort a sign-in running in someone else's
		// direct message, and the confirmation would itself reveal that one is
		// underway. Control of the login stays on the 1:1 channel that owns it.
		if !sub.Private {
			return cancelPrivateOnlyText
		}
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
func (m *Manager) start(base context.Context, force bool, sub Subscriber) string {
	// Refused before anything else: every reply below can carry the one-time code,
	// and subscribing a group destination would also feed it the code when the
	// prompt is broadcast.
	if !sub.Private {
		return LoginPrivateOnlyText
	}
	m.mu.Lock()
	if cur := m.cur; cur != nil {
		subscribeLocked(cur, sub)
		prompt, hasPrompt := cur.prompt, cur.hasPrompt
		m.mu.Unlock()
		// Through ack for the same reason the fresh-start branch below uses it: this
		// caller is already subscribed (a line above), so a reply the ADAPTER sends
		// can be overtaken by a broadcast — leaving "the code arrives in a moment"
		// sitting under a code that already arrived.
		if hasPrompt {
			return m.ack(sub, "A Codex login is already pending — finish this one:\n\n"+promptText(prompt))
		}
		return m.ack(sub, "A Codex login is already starting; the link and code arrive in a moment.")
	}
	// CredentialsPresent, not Authorized: with CODEX_REQUIRE_AUTH=false the policy
	// answer is always "authorized", which would make /login unusable on exactly
	// the deployments most likely to need it.
	if !force && m.CredentialsPresent() {
		m.mu.Unlock()
		return m.StatusText() + "\n\nSend /login force to sign in again."
	}

	// context.WithoutCancel: the login must survive the update whose handler
	// started it. The timeout is the real bound.
	//nolint:gosec // G118: cancel is owned by the session — run defers it, Cancel calls it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(base), m.cfg.timeout())
	cur := &session{cancel: cancel, done: make(chan struct{})}
	subscribeLocked(cur, sub)
	m.cur = cur
	m.mu.Unlock()

	// Delivered through the SAME channel as every later notice, and BEFORE the run
	// goroutine exists. Returning it for the adapter to send separately raced the
	// broadcast: a CLI that printed its banner quickly could get the link and code
	// out first, so the user read "the code arrives in a moment" underneath the
	// code that had already arrived.
	reply := m.ack(sub, "Starting the Codex sign-in — the link and one-time code arrive in a moment.")
	go m.run(ctx, cur)
	return reply
}

// ack delivers an immediate reply through sub, so it cannot be overtaken by a
// notice the caller has not sent yet. It returns the text unsent only when the
// subscriber cannot receive it, leaving the caller to deliver it; an empty return
// means "already delivered, send nothing".
func (m *Manager) ack(sub Subscriber, text string) string {
	if sub.Notify == nil {
		return text
	}
	sub.deliver(text)
	return ""
}

// subscribeLocked adds sub to this attempt's notice list, collapsing a
// destination that is already subscribed. The caller must hold Manager.mu. Only
// private destinations subscribe: the notices carry the one-time code.
func subscribeLocked(s *session, sub Subscriber) {
	if sub.Notify == nil || !sub.Private {
		return
	}
	for _, existing := range s.subs {
		if existing.ID == sub.ID {
			return
		}
	}
	s.subs = append(s.subs, sub)
}

// subscribers snapshots this attempt's destinations, so a delivery (which does
// network I/O) never holds the lock.
func (m *Manager) subscribers(s *session) []Subscriber {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Subscriber(nil), s.subs...)
}

// deliverAll sends text to every destination in subs.
func deliverAll(subs []Subscriber, text string) {
	for _, sub := range subs {
		sub.deliver(text)
	}
}

// run drives one login attempt and reports its result to the destinations waiting
// on THAT attempt. Closing s.done is what lets Cancel wait for the goroutine to
// unwind.
func (m *Manager) run(ctx context.Context, s *session) {
	defer close(s.done)
	defer s.cancel()
	log := m.cfg.logger()

	// Guard against the very symptom this package exists to remove. If the parser
	// never pairs a URL with a code — a changed banner, a code printed with an
	// unexpected prefix — the user would otherwise read "the link and one-time code
	// arrive in a moment" and then hear nothing for DeviceCodeTTL, which is exactly
	// "it just hangs" wearing a friendlier hat. Say so instead, and name the way out.
	warn := time.AfterFunc(promptWarnAfter, func() {
		m.mu.Lock()
		silent := m.cur == s && !s.hasPrompt
		subs := append([]Subscriber(nil), s.subs...)
		m.mu.Unlock()
		if !silent {
			return
		}
		log.Warn("codex printed no device-login code yet", "after", promptWarnAfter)
		deliverAll(subs, noPromptYetText)
	})
	defer warn.Stop()

	err := Login(ctx, m.cfg, func(p Prompt) {
		m.mu.Lock()
		s.prompt, s.hasPrompt = p, true
		m.mu.Unlock()
		// No URL: it pairs with the code, which is why redact strips it from captured
		// output. Codex may also start emitting verification_uri_complete, which
		// embeds the code in the link itself.
		log.Info("codex device login prompt issued", "expires_in", p.ExpiresIn)
		deliverAll(m.subscribers(s), promptText(p))
	})

	// Detach and snapshot in ONE critical section, and only if this attempt is
	// still the current one. A cancelled attempt has already been detached, and a
	// successor may be running by now: without the identity check this would clear
	// the successor's state, and without the snapshot the verdict below would be
	// delivered to whoever is subscribed at that instant — the successor's chat
	// reading a failure as its own, the original never hearing its outcome.
	m.mu.Lock()
	if m.cur == s {
		m.cur = nil
	}
	cancelled := s.cancelled
	subs := append([]Subscriber(nil), s.subs...)
	m.mu.Unlock()

	if cancelled {
		// Cancel already answered the user; the verdict would only repeat it.
		log.Info("codex device login cancelled")
		return
	}
	deliverAll(subs, m.outcome(err, log))
}

// outcome turns a finished attempt into the text its subscribers are told, and
// logs it.
func (m *Manager) outcome(err error, log *slog.Logger) string {
	if err != nil {
		log.Warn("codex device login failed", "error", err)
		return failureText(err)
	}
	// A clean exit is necessary but not sufficient: what actually authorizes the
	// deployment is the credential on disk, and that is what the run gate reads.
	// Announcing success on the exit code alone would tell the user they are
	// signed in and then refuse their very next message.
	if !m.CredentialsPresent() {
		log.Error("codex login exited cleanly but persisted no credential", "codex_home", m.cfg.Home)
		return "The Codex CLI finished the sign-in but saved no credential in " + m.cfg.Home + ".\n\n" +
			"Check that the directory is writable, then send /login to try again."
	}
	log.Info("codex device login succeeded", "codex_home", m.cfg.Home)
	return "Codex is authorized. Send your next message and the team gets to work."
}

// promptWarnAfter is how long the CLI may stay silent before the user is told
// that no code has appeared. A variable so tests need not wait it out.
var promptWarnAfter = 45 * time.Second

// noPromptYetText reports a sign-in that has produced no code yet.
const noPromptYetText = "The Codex CLI still hasn't printed a sign-in code.\n\n" +
	"It may just be slow to start. If nothing arrives shortly, /login cancel stops it and /login tries again."

// cancelDrain bounds how long Cancel waits for the login goroutine to unwind.
const cancelDrain = 5 * time.Second

// Cancel aborts a pending login, reporting whether there was one.
//
// The attempt is detached IMMEDIATELY, before any waiting: from the caller's next
// instruction the Manager is idle, so a /login that follows starts fresh instead
// of being answered "already pending" with the code just killed. The wait that
// follows is only for the goroutine to finish unwinding, and a timeout is
// therefore harmless — there is no state left to be inconsistent. That matters
// because the wait is genuinely reachable: the goroutine may be inside a notice
// delivery, which is bounded by NotifyTimeout, longer than cancelDrain.
//
// Killing the child does NOT depend on that wait either: cancelling the context
// makes Login signal the whole process group at once (SIGTERM, SIGKILL after
// killGrace). So `defer auth.Cancel()` on shutdown still guarantees no orphaned
// polling process, even when the drain times out.
func (m *Manager) Cancel() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	s := m.cur
	if s == nil {
		m.mu.Unlock()
		return false
	}
	s.cancelled = true
	m.cur = nil
	m.mu.Unlock()

	s.cancel()
	select {
	case <-s.done:
	case <-time.After(cancelDrain):
	}
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
