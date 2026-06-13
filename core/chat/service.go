// Package chat is the transport-neutral conversation layer: it receives a text
// message from an allowed user, resolves that chat's isolated workspace, runs
// the message through the claude Runner, and renders the event stream into one
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

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/pending"
)

// tickInterval is how often the wall-clock ticker refreshes the progress frame
// when no event arrives — it advances the elapsed counter/spinner during a silent
// tool call (§7.2). Activity events refresh the live draft immediately (see the run
// loop), so this only governs the quiet-period cadence.
const tickInterval = 1 * time.Second

// minEditInterval throttles real edits in the rate-limited fallback path. Drafts
// (the primary live path) have no Telegram rate limit and refresh every
// tickInterval; the Edit fallback IS rate-limited, so when a run falls back to
// editing the anchor we additionally cap real edits to at most one per
// minEditInterval. A 1s ticker would otherwise hammer the throttled Edit endpoint,
// drawing 429s whose back-off parks render() and freezes the counter. Drafts are
// never throttled by this — only the fallback edit branch.
const minEditInterval = 3 * time.Second

// anchorText is the persistent message sent at the start of a run. It carries the
// Stop button (which an ephemeral draft can't) and is later edited into the final
// answer; the live progress streams separately as a draft.
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
	Cancel(chatID ChatID)
}

// sessionStore persists each chat's Claude session_id so the next message can
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

// pendingStore persists each chat's "interrupted run" marker so a run killed
// mid-flight (e.g. SIGKILL after the drain window on a deploy) is auto-resumed on
// the next startup. *pending.FileStore satisfies it. The marker is written at run
// START (as early as possible) and cleared in finish on EVERY terminal path, so a
// stopped or finished run is never resumed and the run-start write is the only
// writer. A nil store disables the feature (all three calls are safe no-ops).
type pendingStore interface {
	Set(chatID ChatID, marker pending.Marker) error
	Delete(chatID ChatID) error
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
	runner     claude.Runner
	chat       Transport
	caps       Capabilities
	retryAfter retryAfterFunc
	maxRunes   int
	dispatch   dispatcher
	workspace  workspaceEnsurer
	sessions   sessionStore
	pending    pendingStore
	costs      *cost.Store
	outbox     *Sweeper
	nudge      *starNudge
	opts       claude.Options
	timeout    time.Duration
	log        *slog.Logger
	tick       time.Duration
	nowFunc    func() time.Time

	mu      sync.Mutex           // guards runChat and lastMsg
	runChat map[string]ChatID    // active runID -> chatID, for mapping Stop back to a chat
	lastMsg map[ChatID]MessageID // chatID -> the source message id of its latest submitted run
	runSeq  atomic.Uint64
}

// Config bundles the dependencies of a Service.
type Config struct {
	Runner     claude.Runner
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
	// Outbox, when non-nil, sweeps the per-chat outbox directory after each run
	// and delivers any files a run produced as Telegram documents. Nil disables
	// the feature: the text-only finish path stays byte-identical.
	Outbox *Sweeper
	// StarNudge configures the post-task GitHub star nudge. When its Enabled flag
	// is false (the default for non-GitHub deploys) the whole nudge path is inert.
	StarNudge StarNudgeConfig
	Opts      claude.Options // Model/MaxTurns/Env for every run; Workdir is set per chat
	// Timeout bounds a single run's delivery; 0 means no deadline. On timeout the
	// run is cancelled but the captured session_id is still stored (plan §7.3).
	Timeout time.Duration
	// RetryAfter classifies a transport delivery error as a rate-limit back-off
	// (Telegram supplies its 429 detector here). Nil means the transport has no
	// rate-limit signal, so a delivery error is never treated as throttling.
	RetryAfter func(err error) (time.Duration, bool)
	Logger     *slog.Logger
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
		outbox:     cfg.Outbox,
		nudge:      newStarNudge(cfg.StarNudge, cfg.Transport, log),
		opts:       cfg.Opts,
		timeout:    cfg.Timeout,
		log:        log,
		tick:       tickInterval,
		nowFunc:    time.Now,
		runChat:    map[string]ChatID{},
		lastMsg:    map[ChatID]MessageID{},
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
// (text + image content blocks) enabling Claude vision; an empty images slice
// behaves exactly like Handle (trailing-arg text path). Edit-tracking and
// dispatch semantics are identical to Handle.
func (s *Service) HandleMedia(
	_ context.Context, chatID ChatID, userID int64, msgID MessageID, prompt string, images []claude.ImageInput,
) {
	s.mu.Lock()
	s.lastMsg[chatID] = msgID
	s.mu.Unlock()
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, userID, prompt, images)
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
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, 0, prompt, nil)
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
	_ context.Context, chatID ChatID, userID int64, msgID MessageID, prompt string, images []claude.ImageInput,
) {
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
	}
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, userID, prompt, images)
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
	return true
}

// run drives a single message to completion within the Dispatcher-provided ctx:
// resolve the chat workspace, post the initial progress message, stream events
// into the renderer, edit on a wall-clock tick, and replace the progress message
// with the final (chunked) answer. The ctx is cancelled by Stop / Dispatcher
// shutdown.
//
//nolint:gocyclo // orchestrates the single-run lifecycle with best-effort fallbacks.
func (s *Service) run(ctx context.Context, chatID ChatID, userID int64, prompt string, images []claude.ImageInput) {
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
		if _, sErr := s.chat.Send(ctx, chatID, FinalError(err), "", true); sErr != nil {
			s.log.Error("send workspace error", "error", sErr)
		}
		return
	}
	opts.Workdir = workdir

	// Persist the interrupted-run marker as early as possible — right after the
	// workspace resolves and BEFORE the run can be torn down — so a run killed at
	// the drain deadline (or even before SystemInit) always leaves a resumable
	// prompt behind. The anchor id is not known yet (the anchor is sent below); it
	// is filled in once available. finish clears the marker on every terminal path,
	// and run-start is the ONLY writer, so a finished/stopped run is never resumed.
	startedAt := s.nowFunc()
	s.markPending(chatID, pending.Marker{Prompt: prompt, StartedAt: startedAt.UnixMilli()})

	// Resume this chat's stored Claude session so the run continues its context
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
		if _, sErr := s.chat.Send(ctx, chatID, FinalError(err), "", true); sErr != nil {
			s.log.Error("send start error", "error", sErr)
		}
		return
	}

	start := s.nowFunc()
	prog := NewProgress(func() time.Duration { return s.nowFunc().Sub(start) }, 0)

	// The anchor: a persistent message that carries the Stop button (a draft can't)
	// and is later edited into the final answer. It stays static while live progress
	// streams as a draft; only the fallback path (no draft support) edits it. An
	// empty id means no anchor was created (Send failed) — we can still deliver.
	progressMsgID, err := s.chat.Send(ctx, chatID, anchorText, runID, false)
	if err != nil {
		// Without an anchor we can still deliver the result; keep going.
		s.log.Error("send anchor message", "error", err)
		progressMsgID = ""
	}
	// Record the anchor id on the marker now that it is known, so a restart can
	// best-effort edit the dangling "Working…" message into a resume notice. The
	// prompt was already persisted before runner.Run; this only enriches it.
	if progressMsgID != "" {
		s.markPending(chatID, pending.Marker{
			Prompt: prompt, AnchorMsgID: progressMsgID, StartedAt: startedAt.UnixMilli(),
		})
	}

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	var (
		lastSent       string
		throttledUntil time.Time
		// lastEditAt is the wall-clock time of the last SUCCESSFUL fallback edit; its
		// zero value lets the first fallback edit fire promptly (so a mid-run flip from
		// draft to fallback isn't blocked). Only the edit fallback consults it.
		lastEditAt time.Time
		// draftStreaming holds while we push live progress as an ephemeral draft (the
		// rate-limit-free path). The first failed StreamDraft flips it off and the run
		// falls back to editing the anchor for its remainder.
		draftStreaming = progressMsgID != ""
		finalResult    *claude.RunResult
		finalErr       error
	)

	// Seed the live preview immediately so it appears without waiting for a tick.
	if draftStreaming {
		if err := s.chat.StreamDraft(ctx, chatID, runID, prog.Frame(), true); err != nil {
			draftStreaming = false
			s.log.Debug("draft streaming unsupported; falling back to edits", "error", err)
		} else {
			lastSent = prog.Frame()
		}
	}

	// render pushes the current progress frame: as a rate-limit-free draft preview
	// while draftStreaming holds, else by editing the anchor (rate-limit aware).
	render := func() {
		if progressMsgID == "" {
			return
		}
		frame := prog.Frame()
		if frame == lastSent {
			return
		}
		if draftStreaming {
			err := s.chat.StreamDraft(ctx, chatID, runID, frame, true)
			if err == nil {
				lastSent = frame
				return
			}
			// Drafts unsupported here: edit the anchor for the rest of the run.
			draftStreaming = false
			s.log.Debug("draft stream failed; falling back to edits", "error", err)
		}
		// Fallback: edit the anchor. Skip while rate-limited so we don't hammer the
		// throttled endpoint (which prolongs the back-off and starves final delivery).
		if !throttledUntil.IsZero() && s.nowFunc().Before(throttledUntil) {
			return
		}
		// Also throttle real edits to one per minEditInterval: the 1s ticker would
		// otherwise spam the rate-limited Edit endpoint and provoke the 429s above.
		// The zero value of lastEditAt lets the first fallback edit fire immediately.
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
			case claude.SystemInit:
				// Capture and persist the session_id as soon as it is known, BEFORE
				// any later timeout/cancel can tear the run down. This is the §7.3 fix:
				// even if the run never reaches its Result, the next message resumes.
				s.storeSession(chatID, ev.SessionID)
			case claude.Result:
				finalResult = ev.Result
				if ev.Result != nil {
					// Reconcile with the terminal session_id (it should match the init
					// one, but trust the result envelope as authoritative).
					s.storeSession(chatID, ev.Result.SessionID)
				}
			case claude.RunError:
				finalErr = ev.Err
			default:
				// Fold the activity event into the renderer and, in draft mode, push it
				// to the live preview RIGHT AWAY (drafts have no edit rate limit) so the
				// stream reflects what's happening in real time instead of once a tick.
				// In the edit fallback we leave rendering to the (rate-limited) ticker.
				if prog.Observe(ev) && draftStreaming {
					render()
				}
			}
		}
	}

	// Record this run's cost against the sending user for the cumulative cost cap.
	// Only a run that reached its terminal Result carries a cost; an error,
	// timeout, or Stop with no Result records ZERO (finalResult is nil) and is a
	// no-op. The cap is reactive: this Add may push the user over the cap, in which
	// case their NEXT request is denied (the crossing request still ran).
	s.recordCost(userID, finalResult)

	s.finish(ctx, chatID, progressMsgID, runID, finalResult, finalErr, ctx.Err())

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
}

// recordCost accumulates a completed run's USD cost onto userID's running total
// for the cumulative cost cap. A nil result (error/timeout/Stop with no Result)
// records nothing, and a nil store / non-positive cost is a no-op inside Add. A
// store write failure is logged but never aborts the run — a lost accounting
// write only weakens the cap slightly, it does not break delivery.
func (s *Service) recordCost(userID int64, res *claude.RunResult) {
	if s.costs == nil || res == nil {
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

// markPending records (or refreshes) chatID's interrupted-run marker. A nil
// store is a no-op. A write failure is logged but never aborts the run — a lost
// marker only costs this run its auto-resume, it does not break delivery.
func (s *Service) markPending(chatID ChatID, marker pending.Marker) {
	if s.pending == nil {
		return
	}
	if err := s.pending.Set(chatID, marker); err != nil {
		s.log.Error("persist pending marker", "chat_id", chatID, "error", err)
	}
}

// clearPending removes chatID's interrupted-run marker on a terminal so a
// finished or stopped run is NOT auto-resumed after a later restart. A nil store
// is a no-op; a delete failure is logged but never aborts terminal delivery.
func (s *Service) clearPending(chatID ChatID) {
	if s.pending == nil {
		return
	}
	if err := s.pending.Delete(chatID); err != nil {
		s.log.Error("clear pending marker", "chat_id", chatID, "error", err)
	}
}

// finish renders the terminal message and replaces the progress message with it
// (chunked when the answer exceeds the platform's message size limit).
func (s *Service) finish(
	ctx context.Context, chatID ChatID, progressMsgID MessageID, runID string, res *claude.RunResult, runErr, ctxErr error,
) {
	// Use a background context for delivery: the run ctx may be cancelled by Stop
	// or shutdown, but the final message should still reach the user.
	deliverCtx := context.WithoutCancel(ctx)
	// Clear the interrupted-run marker on every CLEAN terminal (normal Result,
	// RunError, timeout, user Stop / /stop / edit-supersede) so a finished or
	// deliberately stopped run is NOT auto-resumed after a later restart. The ONE
	// exception is a terminal caused by the dispatcher SHUTTING DOWN the process
	// (drain-deadline cancel / Close): that run was killed by a deploy/restart, so
	// its marker must SURVIVE to drive the auto-resume — we keep it and let the next
	// startup re-inject. The two are told apart by the cancellation cause: a
	// per-chat Stop/supersede cancels with context.Canceled, a shutdown with
	// dispatch.ErrShutdown. Run-start is the only writer, so a cleared run stays
	// cleared.
	if !errors.Is(context.Cause(ctx), dispatch.ErrShutdown) {
		s.clearPending(chatID)
	}
	// Clear the live draft preview (empty text) so it doesn't linger beside the
	// persisted answer. Best-effort; harmless if no draft was ever streamed.
	_ = s.chat.StreamDraft(deliverCtx, chatID, runID, "", false)
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
			if sErr := s.deliverWithBackoff(deliverCtx, "send final", func() error {
				_, e := s.chat.Send(deliverCtx, chatID, chunks[0], "", true)
				return e
			}); sErr != nil {
				s.log.Error("send final chunk", "error", sErr)
			}
		}
	} else {
		if sErr := s.deliverWithBackoff(deliverCtx, "send final", func() error {
			_, e := s.chat.Send(deliverCtx, chatID, chunks[0], "", true)
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
