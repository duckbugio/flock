// Package telegram wires the claude Runner to the Telegram transport: it
// receives a text message from an allowed user, resolves that chat's isolated
// workspace, runs the message through the Runner, and renders the event stream
// into one live "Working… (Ns)" progress message with a Stop button, replacing
// it with the final answer on completion. Stage 3 replaces Stage 2's single
// per-process serial guard with a Dispatcher: runs are parallel across chats
// (capped) and serial within a chat, each in its own per-chat workspace.
package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/internal/tgui"
)

// tickInterval is how often the wall-clock ticker re-renders the progress
// message. It is independent of stream events so the elapsed counter advances
// during a silent tool call (§7.2).
const tickInterval = 2 * time.Second

// stopCallbackPrefix is the inline-button callback_data prefix carrying the run
// id so a Stop press maps back to the right in-flight run.
const stopCallbackPrefix = "stop:"

// chat abstracts the Telegram operations the run loop needs, so the loop can be
// driven by a fake in tests. The real implementation wraps *bot.Bot.
type chat interface {
	// Send posts a new message with optional Stop markup and returns its id. When
	// asMarkdown is true the text is rendered as Telegram HTML (with a plain-text
	// fallback on a parse error); false sends it verbatim as plain text.
	Send(ctx context.Context, chatID int64, text string, stopRunID string, asMarkdown bool) (messageID int, err error)
	// Edit updates an existing message's text (and Stop markup, when stopRunID
	// is non-empty; empty clears the markup). asMarkdown behaves as for Send.
	Edit(ctx context.Context, chatID int64, messageID int, text string, stopRunID string, asMarkdown bool) error
	// Delete removes a message; failures are non-fatal to the caller.
	Delete(ctx context.Context, chatID int64, messageID int) error
	// SendDocument uploads a file to the chat as a Telegram document. The data is
	// streamed (the go-telegram lib uploads via multipart), so no token-bearing
	// URL is ever constructed for it. Used by the outbox sweeper to deliver files
	// a run produced.
	SendDocument(ctx context.Context, chatID int64, name string, data io.Reader) error
}

// workspaceEnsurer resolves a chat's isolated workspace path, creating it (and
// rendering CLAUDE.md + agents) on first use. *workspace.Renderer satisfies it;
// tests use a fake.
type workspaceEnsurer interface {
	Ensure(chatID int64) (string, error)
}

// dispatcher submits per-chat jobs (serial within a chat, parallel across chats,
// capped) and cancels a chat's in-flight job. *dispatch.Dispatcher satisfies it.
type dispatcher interface {
	Submit(chatID int64, run func(ctx context.Context))
	Cancel(chatID int64)
}

// sessionStore persists each chat's Claude session_id so the next message can
// resume context via --resume. *session.FileStore satisfies it. A timeout/cancel
// must NOT discard the stored id (plan §7.3), so the run path only ever Get/Set;
// the sole Delete caller is the explicit /new reset (NewSession), never the run
// or timeout path.
type sessionStore interface {
	Get(chatID int64) (sessionID string, ok bool)
	Set(chatID int64, sessionID string) error
	// Delete drops chatID's stored session id so the next message starts fresh
	// (no --resume). Deleting an absent chat is a harmless no-op. Only the
	// explicit /new reset calls this.
	Delete(chatID int64) error
}

// Service runs messages through the Runner and renders progress to a chat. It
// submits each message to the Dispatcher, which enforces per-chat serialization
// and the global concurrency cap, and resolves a per-chat workspace for each run.
type Service struct {
	runner    claude.Runner
	chat      chat
	dispatch  dispatcher
	workspace workspaceEnsurer
	sessions  sessionStore
	costs     *cost.Store
	outbox    *Sweeper
	opts      claude.Options
	timeout   time.Duration
	log       *slog.Logger
	tick      time.Duration
	nowFunc   func() time.Time

	mu      sync.Mutex       // guards runChat and lastMsg
	runChat map[string]int64 // active runID -> chatID, for mapping Stop back to a chat
	lastMsg map[int64]int    // chatID -> the source message id of its latest submitted run
	runSeq  atomic.Uint64
}

// Config bundles the dependencies of a Service.
type Config struct {
	Runner     claude.Runner
	Chat       chat
	Dispatcher dispatcher
	Workspace  workspaceEnsurer
	// Sessions persists chatID -> session_id for --resume continuity. May be nil
	// (continuity disabled: every run starts fresh and nothing is stored).
	Sessions sessionStore
	// Costs accumulates each run's USD cost per user for the cumulative cost cap.
	// May be nil (cost tracking disabled: runs record nothing). A nil *cost.Store
	// is itself a no-op, so either a nil interface value or a nil store is safe.
	Costs *cost.Store
	// Outbox, when non-nil, sweeps the per-chat outbox directory after each run
	// and delivers any files a run produced as Telegram documents. Nil disables
	// the feature: the text-only finish path stays byte-identical.
	Outbox *Sweeper
	Opts   claude.Options // Model/MaxTurns/Env for every run; Workdir is set per chat
	// Timeout bounds a single run's delivery; 0 means no deadline. On timeout the
	// run is cancelled but the captured session_id is still stored (plan §7.3).
	Timeout time.Duration
	Logger  *slog.Logger
}

// New builds a Service from cfg.
func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		runner:    cfg.Runner,
		chat:      cfg.Chat,
		dispatch:  cfg.Dispatcher,
		workspace: cfg.Workspace,
		sessions:  cfg.Sessions,
		costs:     cfg.Costs,
		outbox:    cfg.Outbox,
		opts:      cfg.Opts,
		timeout:   cfg.Timeout,
		log:       log,
		tick:      tickInterval,
		nowFunc:   time.Now,
		runChat:   map[string]int64{},
		lastMsg:   map[int64]int{},
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
// sending Telegram user, carried through so the run can attribute its cost to
// that user for the cumulative per-user cost cap (plan §9b).
func (s *Service) Handle(_ context.Context, chatID, userID int64, msgID int, prompt string) {
	s.HandleMedia(context.Background(), chatID, userID, msgID, prompt, nil)
}

// HandleMedia is Handle with optional image attachments: when images is
// non-empty the run is delivered to the CLI as a stream-json user message
// (text + image content blocks) enabling Claude vision; an empty images slice
// behaves exactly like Handle (trailing-arg text path). Edit-tracking and
// dispatch semantics are identical to Handle.
func (s *Service) HandleMedia(_ context.Context, chatID, userID int64, msgID int, prompt string, images []claude.ImageInput) {
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
func (s *Service) Inject(chatID int64, prompt string) {
	s.dispatch.Submit(chatID, func(ctx context.Context) {
		s.run(ctx, chatID, 0, prompt, nil)
	})
}

// HandleEdit processes an edited Telegram message. When the edit targets the
// chat's latest submitted message (the one currently in-flight or queued), the
// in-flight run is cancelled and a fresh run is submitted with the new text,
// superseding the stale one (plan §4 / §7.4). When the edit targets an older,
// already-superseded or finished message it is treated as a fresh message: a new
// run is submitted so the user still gets an answer to their edit. Deletions are
// not delivered to bots and are not handled here.
func (s *Service) HandleEdit(ctx context.Context, chatID, userID int64, msgID int, prompt string) {
	s.HandleEditMedia(ctx, chatID, userID, msgID, prompt, nil)
}

// HandleEditMedia is HandleEdit with optional image attachments (see
// HandleMedia). An empty images slice behaves exactly like HandleEdit.
func (s *Service) HandleEditMedia(_ context.Context, chatID, userID int64, msgID int, prompt string, images []claude.ImageInput) {
	s.mu.Lock()
	last, ok := s.lastMsg[chatID]
	supersede := ok && last == msgID
	// Only advance the chat's "latest" message id when this edit targets/becomes
	// the latest. Editing an OLDER message (while a newer message's run is in
	// flight) must NOT rewind lastMsg, or a later edit of the newer message would
	// fail to supersede it and queue a duplicate run instead. Telegram message ids
	// increase monotonically per chat, so a larger msgID is genuinely newer.
	if supersede || !ok || msgID > last {
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
func (s *Service) run(ctx context.Context, chatID, userID int64, prompt string, images []claude.ImageInput) {
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
		if _, sErr := s.chat.Send(ctx, chatID, tgui.FinalError(err), "", true); sErr != nil {
			s.log.Error("send workspace error", "error", sErr)
		}
		return
	}
	opts.Workdir = workdir

	// Resume this chat's stored Claude session so the run continues its context
	// (empty = a fresh session). Continuity survives restarts because the store is
	// durable and reloaded on startup.
	if s.sessions != nil {
		if sid, ok := s.sessions.Get(chatID); ok {
			opts.SessionID = sid
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
		if _, sErr := s.chat.Send(ctx, chatID, tgui.FinalError(err), "", true); sErr != nil {
			s.log.Error("send start error", "error", sErr)
		}
		return
	}

	start := s.nowFunc()
	prog := tgui.NewProgress(func() time.Duration { return s.nowFunc().Sub(start) }, 0)

	progressMsgID, err := s.chat.Send(ctx, chatID, prog.Frame(), runID, false)
	if err != nil {
		// Without a progress message we can still deliver the result; keep going.
		s.log.Error("send initial progress", "error", err)
		progressMsgID = 0
	}

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	var (
		lastSent       = prog.Frame()
		throttledUntil time.Time
		finalResult    *claude.RunResult
		finalErr       error
	)

	editFrame := func() {
		if progressMsgID == 0 {
			return
		}
		// While Telegram has us rate-limited, skip edits rather than hammer the
		// throttled endpoint — that only prolongs the back-off and can starve the
		// final answer's delivery budget.
		if !throttledUntil.IsZero() && s.nowFunc().Before(throttledUntil) {
			return
		}
		frame := prog.Frame()
		// Only edit when content changed since the last successful edit, to stay
		// within Telegram's edit rate limits.
		if frame == lastSent {
			return
		}
		if err := s.chat.Edit(ctx, chatID, progressMsgID, frame, runID, false); err != nil {
			if d, ok := retryAfter(err); ok {
				throttledUntil = s.nowFunc().Add(d)
			}
			s.log.Debug("edit progress", "error", err)
			return
		}
		lastSent = frame
	}

loop:
	for {
		select {
		case <-ticker.C:
			// Re-render on the wall clock even if no event arrived, so the counter
			// advances during a silent tool call.
			editFrame()

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
				// Fold activity into the renderer; the wall-clock ticker flushes the
				// frame to Telegram, so we don't edit on every event (rate limiting).
				prog.Observe(ev)
			}
		}
	}

	// Record this run's cost against the sending user for the cumulative cost cap.
	// Only a run that reached its terminal Result carries a cost; an error,
	// timeout, or Stop with no Result records ZERO (finalResult is nil) and is a
	// no-op. The cap is reactive: this Add may push the user over the cap, in which
	// case their NEXT request is denied (the crossing request still ran).
	s.recordCost(userID, finalResult)

	s.finish(ctx, chatID, progressMsgID, finalResult, finalErr, ctx.Err())
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
func (s *Service) storeSession(chatID int64, sessionID string) {
	if s.sessions == nil || sessionID == "" {
		return
	}
	if err := s.sessions.Set(chatID, sessionID); err != nil {
		s.log.Error("persist session", "chat_id", chatID, "error", err)
	}
}

// finish renders the terminal message and replaces the progress message with it
// (chunked when the answer exceeds the Telegram size limit).
func (s *Service) finish(ctx context.Context, chatID int64, progressMsgID int, res *claude.RunResult, runErr, ctxErr error) {
	// Use a background context for delivery: the run ctx may be cancelled by Stop
	// or shutdown, but the final message should still reach the user.
	deliverCtx := context.WithoutCancel(ctx)
	var text string
	switch {
	case ctxErr != nil && res == nil && runErr == nil:
		text = "⏹ Stopped."
	case runErr != nil:
		text = tgui.FinalError(runErr)
	default:
		text = tgui.Final(res)
	}

	chunks := tgui.Chunk(text)

	// Replace the progress message with the first chunk (clearing Stop markup);
	// send any further chunks as new messages.
	if progressMsgID != 0 {
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
	// files the run produced as Telegram documents. This runs on EVERY terminal
	// path (normal Result, RunError, Stop/timeout) because finish is the single
	// terminal funnel, and on deliverCtx so a cancelled run still delivers files
	// produced before the cancel (AC2). A nil sweeper or an empty/absent outbox is
	// a no-op that leaves the text-only behavior byte-identical.
	if s.outbox != nil {
		s.outbox.Sweep(deliverCtx, chatID, s.chat)
	}
}

// retryAfter reports the server-requested back-off when err is a Telegram 429
// (rate limit), flooring a missing/zero value to 1s so callers still wait.
func retryAfter(err error) (time.Duration, bool) {
	var tmr *bot.TooManyRequestsError
	if errors.As(err, &tmr) {
		if d := time.Duration(tmr.RetryAfter) * time.Second; d > 0 {
			return d, true
		}
		return time.Second, true
	}
	return 0, false
}

// deliverWithBackoff runs do, retrying on a Telegram 429 after the server's
// requested delay (bounded by maxWait, a few attempts) so the final answer is
// never lost to a transient rate limit. It returns on success, a non-429 error,
// or ctx cancellation. Used for terminal delivery on a background ctx — never on
// the event loop, which must not block.
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
		d, ok := retryAfter(err)
		if !ok {
			return err
		}
		if d > maxWait {
			d = maxWait
		}
		s.log.Warn("telegram rate-limited; backing off", "what", what, "retry_after", d, "attempt", attempt)
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
