// Package chat is the transport-neutral conversation layer: it receives a text
// message from an allowed user, resolves that chat's isolated workspace, runs
// the message through the configured AI provider Runner, and renders the event stream into one
// live "Working… (Ns)" progress message with a Stop button, replacing it with
// the final answer on completion. Runs are parallel across chats (capped) and
// serial within a chat (core/dispatch), each in its own per-chat workspace. It
// drives any platform through the Transport interface (see transport.go); the
// platform-specific glue lives in adapters/<name>.
package chat

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/pending"
	"github.com/duckbugio/flock/core/verify"
)

// tickInterval is how often the wall-clock ticker re-renders the progress frame —
// it advances the elapsed counter/spinner and flushes any activity folded into the
// ring since the last tick. Activity events only update the in-memory ring; the
// ticker is the sole render trigger (real edits are further throttled to
// minEditInterval), so this is the progress refresh cadence.
const tickInterval = 1 * time.Second

// minEditInterval throttles real edits in the rate-limited fallback path. Drafts
// (the primary live path) have no Telegram rate limit and refresh every
// tickInterval; the Edit fallback IS rate-limited, so when a run falls back to
// editing the anchor we additionally cap real edits to at most one per
// minEditInterval. A 1s ticker would otherwise hammer the throttled Edit endpoint,
// drawing 429s whose back-off parks render() and freezes the counter. Drafts are
// never throttled by this — only the fallback edit branch.
const minEditInterval = 3 * time.Second

// anchorText is the persistent progress message sent at the start of a run. It
// carries the Stop button and is edited IN PLACE with the live "Working… (Ns)"
// frame on each (throttled) tick, then edited into the final answer — so a whole
// run is exactly ONE chat bubble, identical on every platform. This initial text
// shows only until the first tick replaces it with the live frame.
const anchorText = "⏳ Working…"

// workspaceEnsurer resolves a chat's isolated workspace path, creating it (and
// rendering CLAUDE.md + agents) on first use. *workspace.Renderer satisfies it;
// tests use a fake.
type workspaceEnsurer interface {
	Ensure(chatID ChatID) (string, error)
}

// dispatcher submits per-chat jobs (serial within a chat, parallel across chats,
// capped) and cancels a chat's in-flight job. *dispatch.Dispatcher satisfies it.
type dispatcher interface {
	Submit(chatID ChatID, run func(ctx context.Context))
	// TrySubmit is the non-blocking Submit: it returns false (instead of blocking)
	// when the chat's lane is full or the dispatcher is closed, used by the
	// best-effort scheduled-fire path that must never stall on a busy lane.
	TrySubmit(chatID ChatID, run func(ctx context.Context)) bool
	Cancel(chatID ChatID)
}

// sessionStore persists each chat's provider session_id so the next message can
// resume context via --resume. *session.FileStore satisfies it. A timeout/cancel
// must NOT discard the stored id (plan §7.3), so the run path only ever Get/Set
// on those paths. Delete has exactly two callers: the explicit /new reset
// (NewSession) and the run path's self-heal when a RESUMED run ends in an
// is_error result (a likely-stale id) — never a timeout, cancel, or RunError.
type sessionStore interface {
	Get(chatID ChatID) (sessionID string, ok bool)
	Set(chatID ChatID, sessionID string) error
	// Delete drops chatID's stored session id so the next message starts fresh
	// (no --resume). Deleting an absent chat is a harmless no-op. Called by the
	// explicit /new reset and by the run path's is_error-while-resuming self-heal.
	Delete(chatID ChatID) error
}

// pendingStore persists each chat's "interrupted run" marker QUEUE so a run
// killed mid-flight (e.g. SIGKILL after the drain window on a deploy) OR merely
// queued behind a running run is auto-resumed on the next startup.
// *pending.FileStore satisfies it. A marker is enqueued at SUBMIT time (so a
// queued-but-never-started run is captured), enriched with its anchor id once
// known, and cleared BY ID on every clean terminal (so a stale run can't remove a
// newer queued run's marker). The whole lane is cleared at once when the user
// intentionally purges it (Stop / edit-supersede). A nil store disables the
// feature (every call is a safe no-op).
type pendingStore interface {
	Enqueue(chatID ChatID, marker pending.Marker) (string, error)
	SetAnchor(chatID ChatID, id, anchorMsgID string) error
	Remove(chatID ChatID, id string) error
	Clear(chatID ChatID) error
	All() map[ChatID][]pending.Marker
}

// retryAfterFunc classifies a transport delivery error as a rate-limit back-off:
// it returns the duration to wait and true when the error is a "too many
// requests" signal the Service should honor before retrying, or false otherwise.
// It is supplied per transport (Telegram maps its 429 here); a nil func is
// treated as "never a rate limit", which is correct for transports without a
// rate-limit signal.
type retryAfterFunc func(err error) (time.Duration, bool)

// Service runs messages through the Runner and renders progress to a chat. It
// submits each message to the Dispatcher, which enforces per-chat serialization
// and the global concurrency cap, and resolves a per-chat workspace for each run.
type Service struct {
	runner     agent.Runner
	chat       Transport
	caps       Capabilities
	retryAfter retryAfterFunc
	maxRunes   int
	dispatch   dispatcher
	workspace  workspaceEnsurer
	sessions   sessionStore
	pending    pendingStore
	costs      *cost.Store
	costCapUSD float64
	outbox     *Sweeper
	nudge      *starNudge
	postrun    PostRunConfig
	opts       agent.Options
	timeout    time.Duration
	log        *slog.Logger
	tick       time.Duration
	nowFunc    func() time.Time
	auth       RunGate

	mu             sync.Mutex                 // guards runChat, lastMsg, verifyRetry, budgetNotified and snapCache
	runChat        map[string]ChatID          // active runID -> chatID, for mapping Stop back to a chat
	lastMsg        map[ChatID]MessageID       // chatID -> the source message id of its latest submitted run
	verifyRetry    map[ChatID][]string        // repos whose gate failed last round (re-gated even if unchanged)
	budgetNotified map[ChatID]string          // chatID -> UTC day the budget-reached notice was last sent
	snapCache      map[ChatID]verify.Snapshot // post-run repo fingerprints, reused as the next run's "before"
	runSeq         atomic.Uint64
}

// Config bundles the dependencies of a Service.
type Config struct {
	Runner     agent.Runner
	Transport  Transport
	Dispatcher dispatcher
	Workspace  workspaceEnsurer
	// Sessions persists chatID -> session_id for --resume continuity. May be nil
	// (continuity disabled: every run starts fresh and nothing is stored).
	Sessions sessionStore
	// Pending persists each chat's interrupted-run marker so a run killed
	// mid-flight on a deploy is auto-resumed on the next startup. May be nil
	// (auto-resume disabled: nothing is marked or cleared).
	Pending pendingStore
	// Costs accumulates each run's USD cost per user for the cumulative cost cap.
	// May be nil (cost tracking disabled: runs record nothing). A nil *cost.Store
	// is itself a no-op, so either a nil interface value or a nil store is safe.
	Costs *cost.Store
	// CostCapUSD is the cumulative per-user USD cap applied to scheduled fires
	// (InjectScheduled): a creator whose accrued cost is at/over this cap has their
	// due jobs skipped. A non-positive value disables the gate (every scheduled fire
	// is allowed). It does NOT affect the inbound message path, which gates earlier.
	CostCapUSD float64
	// Outbox, when non-nil, sweeps the per-chat outbox directory after each run
	// and delivers any files a run produced as Telegram documents. Nil disables
	// the feature: the text-only finish path stays byte-identical.
	Outbox *Sweeper
	// StarNudge configures the post-task GitHub star nudge. When its Enabled flag
	// is false (the default for non-GitHub deploys) the whole nudge path is inert.
	StarNudge StarNudgeConfig
	// PostRun configures the autonomous post-run loop (deterministic gate
	// verification, the /goal evaluator, and the per-chat autonomy budget). The
	// zero value disables all of it (see PostRunConfig).
	PostRun PostRunConfig
	Opts    agent.Options // Model/MaxTurns/Env for every run; Workdir is set per chat
	// Timeout bounds a single run's delivery; 0 means no deadline. On timeout the
	// run is cancelled but the captured session_id is still stored (plan §7.3).
	Timeout time.Duration
	// RetryAfter classifies a transport delivery error as a rate-limit back-off
	// (Telegram supplies its 429 detector here). Nil means the transport has no
	// rate-limit signal, so a delivery error is never treated as throttling.
	RetryAfter func(err error) (time.Duration, bool)
	// Auth blocks every run while the configured provider is unauthorized (today:
	// a Codex subscription deploy whose interactive sign-in has not happened yet).
	// Nil means no gate — the Claude and API-key paths authenticate from env and
	// have nothing to wait for.
	Auth   RunGate
	Logger *slog.Logger
}

// defaultMaxMessageRunes is the chunk limit used when a Transport reports a
// non-positive Capabilities.MaxMessageRunes. It matches Telegram's 4096-rune cap
// so the default behavior is unchanged.
const defaultMaxMessageRunes = TelegramMaxMessage

// New builds a Service from cfg.
func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	caps := Capabilities{}
	if cfg.Transport != nil {
		caps = cfg.Transport.Capabilities()
	}
	maxRunes := caps.MaxMessageRunes
	if maxRunes <= 0 {
		maxRunes = defaultMaxMessageRunes
	}
	return &Service{
		runner:     cfg.Runner,
		chat:       cfg.Transport,
		caps:       caps,
		retryAfter: cfg.RetryAfter,
		maxRunes:   maxRunes,
		dispatch:   cfg.Dispatcher,
		workspace:  cfg.Workspace,
		sessions:   cfg.Sessions,
		pending:    cfg.Pending,
		costs:      cfg.Costs,
		costCapUSD: cfg.CostCapUSD,
		outbox:     cfg.Outbox,
		nudge:      newStarNudge(cfg.StarNudge, cfg.Transport, log),
		postrun:    cfg.PostRun,
		opts:       cfg.Opts,
		timeout:    cfg.Timeout,
		log:        log,
		tick:       tickInterval,
		nowFunc:    time.Now,
		auth:       cfg.Auth,
		runChat:    map[string]ChatID{},
		lastMsg:    map[ChatID]MessageID{},

		verifyRetry:    map[ChatID][]string{},
		budgetNotified: map[ChatID]string{},
		snapCache:      map[ChatID]verify.Snapshot{},
	}
}

// Handle submits a user message (the Telegram message id is msgID) in chatID to
// the Dispatcher. It returns immediately; the Dispatcher runs the job (serially
// per chat) under a cancelable context. The ctx passed here is the per-update
// context; the run itself uses the Dispatcher's per-chat context so it survives
// the update handler returning.
//
// msgID is recorded as the chat's latest source message so a later edit of that
// same message can be matched and superseded (see HandleEdit). userID is the
// sending user, carried through so the run can attribute its cost to that user
// for the cumulative per-user cost cap (plan §9b).
func (s *Service) Handle(_ context.Context, chatID ChatID, userID int64, msgID MessageID, prompt string) {
	// context.Background is deliberate: the run must outlive the per-update context
	// (it runs under the Dispatcher's per-chat context, not the handler's).
	//nolint:contextcheck // intentionally detached; see doc comment.
	s.HandleMedia(context.Background(), chatID, userID, msgID, prompt, nil)
}

// HandleMedia is Handle with optional image attachments: when images is
// non-empty the run is delivered to the CLI as a stream-json user message
// (text + image content blocks) enabling provider vision; an empty images slice
// behaves exactly like Handle (trailing-arg text path). Edit-tracking and
// dispatch semantics are identical to Handle.
func (s *Service) HandleMedia(
	ctx context.Context, chatID ChatID, userID int64, msgID MessageID, prompt string, images []agent.ImageInput,
) {
	if s.blockSubmit(ctx, chatID) {
		return
	}
	s.mu.Lock()
	s.lastMsg[chatID] = msgID
	// A real user message opens a fresh chapter: the post-run verification retry
	// list starts over (the fix-round counter rides inside fix-up prompts, so a
	// user message naturally resets it to zero).
	delete(s.verifyRetry, chatID)
	s.mu.Unlock()
	// Enqueue the interrupted-run marker BEFORE Submit so a run that is only queued
	// (behind a still-running run in the same chat) when the process is killed on a
	// deploy is captured too — a queued job never reaches run-start to mark itself.
	// The run clears this exact marker by id on its clean terminal.
	id := s.enqueuePending(chatID, prompt)
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, userID, prompt, images, id)
	})
}

// Inject submits an externally-sourced prompt (e.g. a PR comment relayed by the
// poller) into chatID as if it were a user message. Unlike Handle it does not
// touch edit-tracking state, so a synthetic event never interferes with a real
// user's message-edit supersede logic. An injected prompt has no originating
// Telegram user, so its cost is attributed to user 0 (a sentinel that no real
// allow-listed user holds); these synthetic relays are rare and are not rate-/
// cost-gated on the inbound path.
func (s *Service) Inject(chatID ChatID, prompt string) {
	if s.blockSubmit(context.Background(), chatID) {
		return
	}
	// Enqueue before Submit so a poller-injected run is captured for auto-resume
	// too (it should resume if killed mid-flight, same as a user message).
	id := s.enqueuePending(chatID, prompt)
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, 0, prompt, nil, id)
	})
}

// InjectScheduled submits a scheduled (cron) job's prompt into chatID, attributing
// the run to its creator (userID) so the cost accrues to them. It differs from
// Inject (the poller path) on the two axes that matter for an UNATTENDED, recurring
// fire:
//
//   - EPHEMERAL: it does NOT enqueue a pending marker, so a scheduled fire is never
//     auto-resumed on restart. A missed fire is acceptable; a durable marker that
//     replays a stale scheduled prompt after a deploy is the bug we are avoiding.
//   - NON-BLOCKING + cost-gated: it gates on the creator's cumulative cost cap and
//     submits via the non-blocking lane, so a frequent cron behind a long-held lane
//     can neither accumulate detached goroutines nor stall the scheduler.
//
// It returns false (the fire is dropped, best-effort) when the creator is at/over
// the cost cap or when the chat's lane is busy (full buffer / dispatcher closed),
// and true when the run was enqueued. Inject, by contrast, runs as the sentinel
// user 0, is durable (enqueues a marker), and blocks on a full buffer.
func (s *Service) InjectScheduled(chatID ChatID, prompt string, userID int64) bool {
	if s.blockSubmit(context.Background(), chatID) {
		return false
	}
	if s.costs != nil && !s.costs.Allowed(userID, s.costCapUSD) {
		return false
	}
	return s.dispatch.TrySubmit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, userID, prompt, nil, "")
	})
}

// ResumePending re-submits an interrupted run recorded in the pending store,
// REUSING the marker's existing id rather than enqueuing a new one. This is what
// makes startup replay idempotent: the resumed run clears the SAME on-disk marker
// (Remove by id) on its clean terminal instead of creating a duplicate, so a
// crash mid-replay only re-resumes the markers a finished run has not yet cleared.
// It deliberately does not touch edit-tracking state (like Inject), so replay
// never interferes with a real user's supersede logic.
func (s *Service) ResumePending(chatID ChatID, m pending.Marker) {
	// The marker is deliberately left in place: it was not created here, and an
	// interrupted run must not be lost to the outage that is blocking it. It
	// replays after the sign-in, on the next restart.
	if s.blockSubmit(context.Background(), chatID) {
		return
	}
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, 0, m.Prompt, nil, m.ID)
	})
}

// HandleEdit processes an edited message. When the edit targets the chat's
// latest submitted message (the one currently in-flight or queued), the
// in-flight run is cancelled and a fresh run is submitted with the new text,
// superseding the stale one (plan §4 / §7.4). When the edit targets an older,
// already-superseded or finished message it is treated as a fresh message: a new
// run is submitted so the user still gets an answer to their edit. Deletions are
// not delivered to bots and are not handled here.
func (s *Service) HandleEdit(ctx context.Context, chatID ChatID, userID int64, msgID MessageID, prompt string) {
	s.HandleEditMedia(ctx, chatID, userID, msgID, prompt, nil)
}

// HandleEditMedia is HandleEdit with optional image attachments (see
// HandleMedia). An empty images slice behaves exactly like HandleEdit.
func (s *Service) HandleEditMedia(
	ctx context.Context, chatID ChatID, userID int64, msgID MessageID, prompt string, images []agent.ImageInput,
) {
	if s.blockSubmit(ctx, chatID) {
		return
	}
	s.mu.Lock()
	last, ok := s.lastMsg[chatID]
	supersede := ok && last == msgID
	// Only advance the chat's "latest" message id when this edit targets/becomes
	// the latest. Editing an OLDER message (while a newer message's run is in
	// flight) must NOT rewind lastMsg, or a later edit of the newer message would
	// fail to supersede it and queue a duplicate run instead. Message ids increase
	// monotonically per chat, so a "newer" msgID is genuinely later (newerMessage
	// compares numeric ids numerically, preserving the original int64 ordering).
	if supersede || !ok || newerMessage(msgID, last) {
		s.lastMsg[chatID] = msgID
	}
	s.mu.Unlock()

	if supersede {
		// Clear the chat's lane (cancel the in-flight run + drop any still-queued
		// jobs) so the stale message can't run as a duplicate; the in-flight run
		// unwinds with a "Stopped" terminal, then the resubmitted job runs.
		s.dispatch.Cancel(chatID)
		// Cancel purges the whole lane, so its enqueued markers must go too —
		// otherwise the dropped queued runs would auto-resume on a later restart. The
		// cancelled run's async finish clears its own marker BY ID, so it cannot touch
		// the new successor enqueued below.
		s.clearLanePending(chatID)
	}
	// Enqueue the new marker after any lane purge so the resubmitted run is captured
	// for auto-resume; the run clears this exact id on its clean terminal.
	id := s.enqueuePending(chatID, prompt)
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, userID, prompt, images, id)
	})
}

// Stop halts a chat's work when runID matches: it cancels the in-flight run and
// purges that chat's queued-but-not-started jobs (via Dispatcher.Cancel), so
// nothing pending runs after the user hits Stop. It is safe to call from a
// Telegram callback handler and reports whether a matching run was found.
func (s *Service) Stop(runID string) bool {
	s.mu.Lock()
	chatID, ok := s.runChat[runID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	s.dispatch.Cancel(chatID)
	// Purge the whole lane's markers: Stop cancels the running run AND drops any
	// still-queued jobs, and the user does not want any of them resumed (there is
	// no resubmit to preserve).
	s.clearLanePending(chatID)
	return true
}

// run drives a single message to completion within the Dispatcher-provided ctx:
// resolve the chat workspace, post the initial progress message, stream events
// into the renderer, edit on a wall-clock tick, and replace the progress message
// with the final (chunked) answer. The ctx is cancelled by Stop / Dispatcher
// shutdown.
//
//nolint:gocyclo // orchestrates the single-run lifecycle with best-effort fallbacks.
func (s *Service) run(
	ctx context.Context, chatID ChatID, userID int64, prompt string, images []agent.ImageInput, markerID string,
) {
	runID := strconv.FormatUint(s.runSeq.Add(1), 10)
	s.mu.Lock()
	s.runChat[runID] = chatID
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.runChat, runID)
		s.mu.Unlock()
	}()

	// Resolve this chat's isolated workspace (per-chat CLAUDE.md + agents) and
	// run the CLI with it as the working dir.
	opts := s.opts
	// A media run carries image content blocks; the Runner switches to stream-json
	// stdin delivery when this is non-empty (vision), else uses the text path.
	opts.Images = images
	workdir, err := s.workspace.Ensure(chatID)
	if err != nil {
		s.log.Error("ensure workspace", "chat_id", chatID, "error", err)
		// A marker was enqueued at submit, but this run never starts — clear it (by
		// id, gated on shutdown like finish) so a workspace failure is not resumed.
		s.removePendingUnlessShutdown(ctx, chatID, markerID)
		if _, sErr := s.chat.Send(ctx, chatID, FinalError(err), "", true); sErr != nil {
			s.log.Error("send workspace error", "error", sErr)
		}
		return
	}
	opts.Workdir = workdir

	// The post-run verifier needs each repo's state as of BEFORE this run. Reuse
	// the fingerprint cached at the END of the previous run (refreshSnapCache) so
	// the git calls stay off the per-message critical path; only the first run
	// after boot pays a synchronous Take here. Nil verifier → nil snapshot.
	var preSnap verify.Snapshot
	if s.postrun.Verifier != nil {
		s.mu.Lock()
		cached, ok := s.snapCache[chatID]
		s.mu.Unlock()
		if ok {
			preSnap = cached
		} else {
			preSnap = s.postrun.Verifier.Take(ctx, workdir)
		}
	}
	// The dispatcher lane context, captured before the per-run timeout is layered
	// on: post-run work (verify/evaluate) must not inherit a deadline the main run
	// already spent, but must still die on Stop/shutdown.
	laneCtx := ctx

	// The interrupted-run marker already exists: it was enqueued at SUBMIT time (so
	// a still-queued run is captured too), not at run start. This run clears that
	// exact marker by id on its clean terminal, and enriches it with the anchor id
	// once the anchor is sent below. A shutdown-caused terminal keeps the marker so
	// the next startup auto-resumes it.

	// Resume this chat's stored provider session so the run continues its context
	// (empty = a fresh session). Continuity survives restarts because the store is
	// durable and reloaded on startup. resuming records whether THIS run carried a
	// non-empty resume id, so a terminal is_error Result can self-heal a stale/
	// poisoned session id (see the clear after the event loop).
	resuming := false
	if s.sessions != nil {
		if sid, ok := s.sessions.Get(chatID); ok && sid != "" {
			opts.SessionID = sid
			resuming = true
		}
	}

	// Apply the per-run delivery deadline. The timeout cancels the run (and thus
	// delivery), but the session_id captured below is still persisted, so the next
	// message resumes seamlessly — the §7.3 wart fix. We derive the deadline from
	// the dispatcher ctx so a Stop/shutdown still cancels promptly.
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	events, err := s.runner.Run(ctx, prompt, opts)
	if err != nil {
		s.log.Error("start run", "error", err)
		// A spawn failure is a clean (non-shutdown) terminal that never reaches
		// finish, so clear this run's marker by id — gated exactly like finish so a
		// shutdown-caused start failure still keeps its marker and auto-resumes.
		s.removePendingUnlessShutdown(ctx, chatID, markerID)
		if _, sErr := s.chat.Send(ctx, chatID, FinalError(err), "", true); sErr != nil {
			s.log.Error("send start error", "error", sErr)
		}
		return
	}

	start := s.nowFunc()
	prog := NewProgress(func() time.Duration { return s.nowFunc().Sub(start) }, 0, workdir)

	// The anchor: a persistent message that carries the Stop button and is edited in
	// place with the live progress frame, then with the final answer. An empty id
	// means no anchor was created (Send failed) — we can still deliver.
	progressMsgID, err := s.chat.Send(ctx, chatID, anchorText, runID, false)
	if err != nil {
		// Without an anchor we can still deliver the result; keep going.
		s.log.Error("send anchor message", "error", err)
		progressMsgID = ""
	}
	// Record the anchor id on the marker now that it is known, so a restart can
	// best-effort edit the dangling "Working…" message into a resume notice. The
	// prompt was already persisted at submit; this only enriches the same entry by
	// id (a no-op if the marker was already cleared).
	if progressMsgID != "" {
		s.setAnchorPending(chatID, markerID, progressMsgID)
	}

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	var (
		lastSent       string
		throttledUntil time.Time
		// lastEditAt is the wall-clock time of the last SUCCESSFUL progress edit; its
		// zero value lets the first edit fire promptly.
		lastEditAt  time.Time
		finalResult *agent.RunResult
		finalErr    error
	)

	// render edits the single anchor message in place with the current progress
	// frame, rate-limit aware. The anchor IS the one live progress bubble (it also
	// carries the Stop button), so a run is exactly ONE message on every platform.
	// Telegram caps message edits, so real edits are throttled to one per
	// minEditInterval; the 1s ticker only advances the in-memory counter, and most
	// ticks early-return on frame==lastSent or the throttle.
	render := func() {
		if progressMsgID == "" {
			return
		}
		frame := prog.Frame()
		if frame == lastSent {
			return
		}
		// Skip while rate-limited so we don't hammer the throttled endpoint (which
		// prolongs the back-off and starves final delivery).
		if !throttledUntil.IsZero() && s.nowFunc().Before(throttledUntil) {
			return
		}
		// Throttle real edits to one per minEditInterval: the 1s ticker would otherwise
		// spam the rate-limited Edit endpoint and provoke 429s. The zero value of
		// lastEditAt lets the first edit fire immediately.
		if !lastEditAt.IsZero() && s.nowFunc().Sub(lastEditAt) < minEditInterval {
			return
		}
		if err := s.chat.Edit(ctx, chatID, progressMsgID, frame, runID, true); err != nil {
			if d, ok := s.retryAfterErr(err); ok {
				throttledUntil = s.nowFunc().Add(d)
			}
			s.log.Debug("edit progress", "error", err)
			return
		}
		lastEditAt = s.nowFunc()
		lastSent = frame
	}

loop:
	for {
		select {
		case <-ticker.C:
			// Re-render on the wall clock even if no event arrived, so the counter
			// advances during a silent tool call.
			render()

		case ev, ok := <-events:
			if !ok {
				break loop
			}
			switch ev.Type {
			case agent.SystemInit:
				// Capture and persist the session_id as soon as it is known, BEFORE
				// any later timeout/cancel can tear the run down. This is the §7.3 fix:
				// even if the run never reaches its Result, the next message resumes.
				s.storeSession(chatID, ev.SessionID)
			case agent.Result:
				finalResult = ev.Result
				if ev.Result != nil {
					// Reconcile with the terminal session_id (it should match the init
					// one, but trust the result envelope as authoritative).
					s.storeSession(chatID, ev.Result.SessionID)
				}
			case agent.RunError:
				finalErr = ev.Err
			default:
				// Fold the activity event into the renderer. Rendering is left to the
				// (rate-limited) ticker — the Edit endpoint can't keep up with per-event
				// edits, so an event only updates the in-memory ring and the next tick
				// flushes it (throttled).
				prog.Observe(ev)
			}
		}
	}

	// Record this run's cost: against the sending user for the cumulative cost
	// cap, or — for an autonomous run (AutoUser) — against the chat's per-day
	// autonomy budget. Only a run that reached its terminal Result carries a cost;
	// an error, timeout, or Stop with no Result records ZERO (finalResult is nil)
	// and is a no-op. Both caps are reactive: this Add may push over the cap, in
	// which case the NEXT request is denied (the crossing request still ran).
	s.recordCost(chatID, userID, finalResult)

	s.finish(ctx, chatID, progressMsgID, markerID, finalResult, finalErr, ctx.Err(), s.nowFunc().Sub(start))

	// Self-heal a stale/poisoned resume id: when a run that USED a resume session
	// id terminates with an is_error Result, drop the stored session for this chat
	// so the NEXT message starts fresh (no --resume) instead of re-failing forever.
	// This fires ONLY on an is_error Result while resuming — never on a clean
	// Result, a RunError (a process/transport crash, likely transient — keep the
	// context for a retry), or a Stop/timeout (finalResult nil). storeSession may
	// have just re-persisted the id from this Result, so clear it here regardless.
	if resuming && finalErr == nil && ctx.Err() == nil && finalResult != nil && finalResult.IsError {
		s.log.Info("clearing session after error during resume; next message starts fresh", "chat_id", chatID)
		if err := s.sessions.Delete(chatID); err != nil {
			s.log.Error("clear session after error", "chat_id", chatID, "error", err)
		}
	}

	// Post-task GitHub star nudge. Fire ONLY on a cleanly successful run (a
	// terminal Result with no error and no cancel/timeout) so a failed, stopped,
	// or timed-out run never nudges. The nudge runs fully off the hot path (its
	// own detached goroutine, recovering any panic) so it can never delay or break
	// a run; a nil/disabled nudge makes this a no-op.
	if finalResult != nil && finalErr == nil && ctx.Err() == nil {
		// maybeNudge detaches its own context (it outlives this run's ctx by design).
		//nolint:contextcheck // intentionally detached; the nudge runs post-completion.
		s.nudge.maybeNudge(chatID)
	}

	// The autonomy tail (deterministic gate verification, then the /goal
	// evaluator) fires ONLY on a cleanly successful run — never on an error
	// result, a RunError, a Stop, or a timeout. It runs on the lane context
	// (pre-timeout) so it is not starved by a deadline the main run already
	// spent, yet still dies on Stop/shutdown. It runs synchronously on the lane
	// so its injected follow-up is ordered before any queued user message.
	if finalResult != nil && finalErr == nil && ctx.Err() == nil && !finalResult.IsError {
		s.postRun(laneCtx, chatID, workdir, prompt, preSnap)
	}

	// Cache the workspace's post-run fingerprints as the NEXT run's "before"
	// state. This runs on EVERY terminal (an errored or stopped run may still
	// have mutated repos) and after the answer is already delivered, so the git
	// calls never add to the user's perceived latency.
	s.refreshSnapCache(laneCtx, chatID, workdir)

	// Schedule any follow-up files the run wrote (the legal "come back later"),
	// then — only on a clean success that scheduled nothing — check the final
	// answer for an unbacked "I'll report back" promise and nudge once.
	scheduled := s.sweepFollowups(laneCtx, chatID, workdir)
	if scheduled == 0 && finalResult != nil && finalErr == nil && ctx.Err() == nil && !finalResult.IsError {
		s.maybeNudgePromise(laneCtx, chatID, prompt, finalResult.Text)
	}
}

// refreshSnapCache fingerprints the workspace and stores the result as
// chatID's cached pre-run snapshot. A nil verifier is a no-op.
func (s *Service) refreshSnapCache(ctx context.Context, chatID ChatID, workdir string) {
	if s.postrun.Verifier == nil {
		return
	}
	snap := s.postrun.Verifier.Take(ctx, workdir)
	s.mu.Lock()
	s.snapCache[chatID] = snap
	s.mu.Unlock()
}

// recordCost accumulates a completed run's USD cost: a real user's run onto
// their cumulative cost-cap total, an autonomous run (AutoUser) onto the chat's
// per-day autonomy budget. The 0 sentinel (poller relays and startup resumes,
// whose original sender is unknown) records nowhere, preserving its old
// behavior. A nil result (error/timeout/Stop with no Result) records nothing,
// and a nil store / non-positive cost is a no-op inside Add. A store write
// failure is logged but never aborts the run — a lost accounting write only
// weakens the cap slightly, it does not break delivery.
func (s *Service) recordCost(chatID ChatID, userID int64, res *agent.RunResult) {
	if userID == AutoUser {
		s.recordAutoCost(chatID, res)
		return
	}
	if s.costs == nil || res == nil || userID <= 0 {
		return
	}
	if err := s.costs.Add(userID, res.CostUSD); err != nil {
		s.log.Error("record run cost", "user_id", userID, "error", err)
	}
}

// storeSession persists the latest session_id for chatID so the next message
// resumes this chat's context. Empty ids and a nil store are ignored. A store
// write failure is logged but never aborts the run — a lost id only costs the
// next message its resume, it does not break delivery. Crucially this is called
// on SystemInit (not only on Result), so a run that later times out or is
// stopped still leaves a resumable session behind (plan §7.3).
func (s *Service) storeSession(chatID ChatID, sessionID string) {
	if s.sessions == nil || sessionID == "" {
		return
	}
	if err := s.sessions.Set(chatID, sessionID); err != nil {
		s.log.Error("persist session", "chat_id", chatID, "error", err)
	}
}

// enqueuePending appends a fresh interrupted-run marker for chatID at submit time
// and returns its process-unique id (used to clear the exact entry on a
// terminal). A nil store or a write failure yields "" (the run still proceeds; a
// lost marker only costs it its auto-resume, it does not break delivery).
func (s *Service) enqueuePending(chatID ChatID, prompt string) string {
	if s.pending == nil {
		return ""
	}
	id, err := s.pending.Enqueue(chatID, pending.Marker{Prompt: prompt, StartedAt: s.nowFunc().UnixMilli()})
	if err != nil {
		s.log.Error("enqueue pending marker", "chat_id", chatID, "error", err)
		return ""
	}
	return id
}

// setAnchorPending records the anchor message id on the marker with the given id
// so a restart can edit the dangling "Working…" message into a resume notice. A
// nil store, an empty id, or a missing marker is a harmless no-op; a write
// failure is logged but never aborts the run.
func (s *Service) setAnchorPending(chatID ChatID, id, anchorMsgID string) {
	if s.pending == nil || id == "" {
		return
	}
	if err := s.pending.SetAnchor(chatID, id, anchorMsgID); err != nil {
		s.log.Error("set pending anchor", "chat_id", chatID, "error", err)
	}
}

// removePendingUnlessShutdown clears this run's marker BY ID on a CLEAN terminal
// but keeps it when the terminal was caused by the dispatcher SHUTTING DOWN the
// process (drain-deadline cancel / Close): such a run was killed by a
// deploy/restart, so its marker must SURVIVE to drive the auto-resume. The two
// are told apart by the cancellation cause — a per-chat Stop/supersede cancels
// with context.Canceled, a shutdown with dispatch.ErrShutdown. Clearing by id
// means a stale/cancelled run can never remove a newer queued run's marker. A nil
// store or an empty id is a no-op; a remove failure is logged but never aborts
// terminal delivery.
func (s *Service) removePendingUnlessShutdown(ctx context.Context, chatID ChatID, id string) {
	if s.pending == nil || id == "" {
		return
	}
	if errors.Is(context.Cause(ctx), dispatch.ErrShutdown) {
		return
	}
	if err := s.pending.Remove(chatID, id); err != nil {
		s.log.Error("clear pending marker", "chat_id", chatID, "error", err)
	}
}

// removePending clears this run's marker BY ID unconditionally (regardless of the
// cancellation cause). Used by finish for a run that REACHED a terminal — it
// produced a Result or errored — so its marker must NOT survive even when the ctx
// was shutdown-cancelled in the microsecond window at the drain deadline;
// otherwise the already-finished run would be phantom-resumed (a duplicate run
// that can re-trigger side effects). A nil store or an empty id is a no-op; a
// remove failure is logged but never aborts terminal delivery.
func (s *Service) removePending(chatID ChatID, id string) {
	if s.pending == nil || id == "" {
		return
	}
	if err := s.pending.Remove(chatID, id); err != nil {
		s.log.Error("clear pending marker", "chat_id", chatID, "error", err)
	}
}

// clearLanePending purges ALL of chatID's interrupted-run markers (the whole
// lane) unconditionally. Used by Stop/StopChat/edit-supersede, where the user
// intentionally drops every queued+running job for the chat and there is no
// resubmit to preserve. A nil store is a no-op; a failure is logged but never
// aborts the caller.
func (s *Service) clearLanePending(chatID ChatID) {
	if s.pending == nil {
		return
	}
	if err := s.pending.Clear(chatID); err != nil {
		s.log.Error("clear pending lane", "chat_id", chatID, "error", err)
	}
}

// finish renders the terminal message and replaces the progress message with it
// (chunked when the answer exceeds the platform's message size limit).
func (s *Service) finish(
	ctx context.Context, chatID ChatID, progressMsgID MessageID, markerID string,
	res *agent.RunResult, runErr, ctxErr error, total time.Duration,
) {
	// Use a background context for delivery: the run ctx may be cancelled by Stop
	// or shutdown, but the final message should still reach the user.
	deliverCtx := context.WithoutCancel(ctx)
	// Clear this run's interrupted-run marker by id whenever the run REACHED a
	// terminal — it delivered a Result or errored — so a finished run is never
	// auto-resumed after a later restart. The marker is KEPT only for a run that
	// produced NOTHING (no Result, no error) and was shutdown-cancelled: that run
	// was genuinely interrupted mid-flight by a deploy/restart, so its marker must
	// survive to drive the auto-resume. Gating on the cancellation cause alone is
	// not enough — a run that reached a clean terminal and then had its ctx
	// shutdown-cancelled in the microsecond window at the drain deadline would
	// otherwise keep its marker and be phantom-resumed (a duplicate run that can
	// re-trigger side effects). Clearing by id means this run can never remove a
	// newer queued run's marker.
	genuinelyInterrupted := res == nil && runErr == nil &&
		errors.Is(context.Cause(ctx), dispatch.ErrShutdown)
	if !genuinelyInterrupted {
		s.removePending(chatID, markerID)
	}
	var text string
	switch {
	case ctxErr != nil && res == nil && runErr == nil:
		text = "⏹ Stopped."
	case runErr != nil:
		text = FinalError(runErr)
	default:
		text = Final(res)
	}

	// Fence-aware chunking so a code block that crosses a chunk boundary stays
	// renderable (balanced ``` per chunk) on both the rich and legacy paths.
	chunks := ChunkFencedSize(text, s.maxRunes)

	// Track the id of the message that ends up holding the first answer chunk, so the
	// completion notice can be sent as a native reply to it (tapping the "done" ping
	// jumps to the answer). It starts as the run's own anchor; the delete+resend
	// fallback and the no-anchor path below update it to the actual answer message id,
	// and it stays "" if every send failed (then the notice is sent plain).
	answerMsgID := progressMsgID

	// Replace the progress message with the first chunk (clearing Stop markup);
	// send any further chunks as new messages.
	//nolint:nestif // the edit-then-resend fallback is a single cohesive delivery path.
	if progressMsgID != "" {
		if err := s.deliverWithBackoff(deliverCtx, "edit final", func() error {
			return s.chat.Edit(deliverCtx, chatID, progressMsgID, chunks[0], "", true)
		}); err != nil {
			// The edit failed for a non-rate-limit reason (a 429 is retried inside
			// deliverWithBackoff, never dropped): delete the progress message and
			// resend so the answer still lands.
			s.log.Debug("edit final; deleting + resending", "error", err)
			if dErr := s.chat.Delete(deliverCtx, chatID, progressMsgID); dErr != nil {
				s.log.Debug("delete progress", "error", dErr)
			}
			// The anchor is gone; the answer now lives in the resent message, so the
			// notice must reply to THAT id (or none, if the resend also failed).
			answerMsgID = ""
			if sErr := s.deliverWithBackoff(deliverCtx, "send final", func() error {
				id, e := s.chat.Send(deliverCtx, chatID, chunks[0], "", true)
				if e == nil {
					answerMsgID = id
				}
				return e
			}); sErr != nil {
				s.log.Error("send final chunk", "error", sErr)
			}
		}
	} else {
		// No anchor existed (the initial progress Send failed): the first chunk is a
		// fresh Send, so the notice replies to its returned id.
		if sErr := s.deliverWithBackoff(deliverCtx, "send final", func() error {
			id, e := s.chat.Send(deliverCtx, chatID, chunks[0], "", true)
			if e == nil {
				answerMsgID = id
			}
			return e
		}); sErr != nil {
			s.log.Error("send final chunk", "error", sErr)
		}
	}

	for _, c := range chunks[1:] {
		if sErr := s.deliverWithBackoff(deliverCtx, "send final", func() error {
			_, e := s.chat.Send(deliverCtx, chatID, c, "", true)
			return e
		}); sErr != nil {
			s.log.Error("send final chunk", "error", sErr)
		}
	}

	// After the text answer is delivered, sweep the per-chat outbox and send any
	// files the run produced as documents. This runs on EVERY terminal path
	// (normal Result, RunError, Stop/timeout) because finish is the single terminal
	// funnel, and on deliverCtx so a cancelled run still delivers files produced
	// before the cancel (AC2). A nil sweeper or an empty/absent outbox is a no-op
	// that leaves the text-only behavior byte-identical. Platforms that cannot send
	// documents (Capabilities.CanSendDocument == false) skip the sweep; Telegram
	// reports true, so its behavior is unchanged.
	if s.outbox != nil && s.caps.CanSendDocument {
		s.outbox.Sweep(deliverCtx, chatID, s.chat)
	}

	// Send a SEPARATE completion message carrying the run's total elapsed time. The
	// in-place edit above never notifies (Telegram is silent on edits), so this is a
	// real second Send that pings the user the run is done — once per run, distinct
	// from the answer and never attached to the live "Working…" edit. A run cancelled
	// by a deploy shutdown keeps its marker and auto-resumes on the next startup, so
	// it is reported as paused (not stopped) to avoid a contradictory signal. The
	// notice is the deliverable that must reach the user, so it uses the same
	// rate-limit backoff as the answer rather than a bare Send that a transient 429
	// would silently drop.
	// Reply the notice to this run's own answer/anchor message so tapping the "done"
	// ping jumps to the answer bubble. When no usable answer id exists (every send
	// failed) fall back to a plain Send so the notice still reaches the user. Either
	// way it fires once, through the same 429-aware backoff as the answer.
	notice := completionNotice(res, runErr, ctxErr, total, genuinelyInterrupted)
	if sErr := s.deliverWithBackoff(deliverCtx, "completion notice", func() error {
		if answerMsgID != "" {
			_, e := s.chat.SendReply(deliverCtx, chatID, answerMsgID, notice, true)
			return e
		}
		_, e := s.chat.Send(deliverCtx, chatID, notice, "", true)
		return e
	}); sErr != nil {
		s.log.Error("send completion notice", "error", sErr)
	}
}

// completionNotice renders the one-line "done" message sent as a separate bubble
// after the answer, carrying the run's total elapsed time with a status word that
// mirrors the terminal cases finish handles: a run cancelled by a deploy shutdown
// (resuming) keeps its marker and auto-resumes, so it reads "paused … will resume";
// a user Stop / per-run timeout (ctxErr set, no Result or error) reads "stopped"; a
// RunError or an is_error Result reads "failed"; and a clean Result reads "done".
func completionNotice(res *agent.RunResult, runErr, ctxErr error, total time.Duration, resuming bool) string {
	elapsed := formatElapsed(int64(total / time.Second))
	switch {
	case resuming:
		return "⏳ Paused after " + elapsed + " — will resume after restart"
	case ctxErr != nil && res == nil && runErr == nil:
		return "⏹ Stopped after " + elapsed
	case runErr != nil || (res != nil && res.IsError):
		return "⚠️ Failed after " + elapsed
	default:
		return "✅ Done in " + elapsed
	}
}

// retryAfterErr classifies a transport delivery error as a rate-limit back-off
// via the per-transport RetryAfter func. A nil func (transport without a
// rate-limit signal) reports "not a rate limit".
func (s *Service) retryAfterErr(err error) (time.Duration, bool) {
	if s.retryAfter == nil {
		return 0, false
	}
	return s.retryAfter(err)
}

// deliverWithBackoff runs do, retrying on a transport rate-limit signal after the
// requested delay (bounded by maxWait, a few attempts) so the final answer is
// never lost to a transient rate limit. It returns on success, a non-rate-limit
// error, or ctx cancellation. Used for terminal delivery on a background ctx —
// never on the event loop, which must not block.
func (s *Service) deliverWithBackoff(ctx context.Context, what string, do func() error) error {
	const (
		maxAttempts = 4
		maxWait     = 30 * time.Second
	)
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = do(); err == nil {
			return nil
		}
		d, ok := s.retryAfterErr(err)
		if !ok {
			return err
		}
		if d > maxWait {
			d = maxWait
		}
		s.log.Warn("transport rate-limited; backing off", "what", what, "retry_after", d, "attempt", attempt)
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
	return err
}

// newerMessage reports whether message id a is later than b within a chat.
// Platform message ids increase monotonically per chat; Telegram (and VK peer)
// ids are numeric, so when both parse as integers they are compared numerically
// — preserving the original int64 ordering exactly (so "9" is older than "10").
// Ids that are not both numeric fall back to a lexicographic compare.
func newerMessage(a, b MessageID) bool {
	ai, aErr := strconv.ParseInt(a, 10, 64)
	bi, bErr := strconv.ParseInt(b, 10, 64)
	if aErr == nil && bErr == nil {
		return ai > bi
	}
	return a > b
}
