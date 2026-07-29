package chat

import "context"

// RunGate reports whether the configured AI provider can run at all right now.
// It exists for one case today: a Codex subscription deployment that has not
// completed its interactive sign-in yet (core/codexauth.Manager satisfies it).
//
// The gate lives HERE, at the submit paths every run enters through, rather than
// only in the adapters' message paths. Runs reach the provider from six places —
// a user message, an edit of one, a poller-injected PR comment, a cron fire, the
// autonomy path (workspace follow-ups, CI events, verify/goal fix-ups) and the
// restart replay of an interrupted run — and only the first two go through an
// adapter. Gating in the adapters alone would let the rest march into a CLI that
// cannot authenticate, once per tick, with nothing user-visible to explain it.
//
// Every one of those six calls blockSubmit; a seventh submit path must call it
// too. TestGateBlocksEveryRunSource enumerates them.
type RunGate interface {
	// BlockedNotice returns the user-facing explanation and true when runs must
	// be blocked, or ("", false) when they may proceed.
	BlockedNotice() (string, bool)
}

// RunsBlocked reports whether a run submitted right now would be refused. It
// lets a caller that would DESTROY work by submitting — the follow-up sweep takes
// each due item out of its store before firing it — skip the attempt entirely and
// try again later, instead of handing it to a gate that drops it.
func (s *Service) RunsBlocked() bool {
	if s.auth == nil {
		return false
	}
	_, blocked := s.auth.BlockedNotice()
	return blocked
}

// blockSubmit reports whether a submission must be refused because the provider
// is unauthorized, and tells the chat once why.
//
// It is called at SUBMIT time, before a pending marker would be enqueued. That
// ordering is the point: blocking later, inside the run, would leave behind a
// marker for work that never started — and since the pending store is append-only
// and a blocked run can never reach the clean terminal that clears it, a poller
// or cron firing into an unauthorized deployment would accumulate markers without
// bound and replay every one of them on the next restart. Refusing before the
// marker exists means there is nothing to leak. ResumePending is the deliberate
// exception: its marker came from a previous boot, so it is left untouched and
// replays once the sign-in lands.
//
// The notice is sent at most ONCE per chat per unauthorized period: background
// sources are recurring (a five-minute cron would otherwise post a notice every
// five minutes), and the adapters already answer a user's own message directly.
// The refusal is logged every time, so the operator sees the full picture in the
// logs while the chat stays readable. The counter resets as soon as the provider
// is authorized again, so a later lapse is announced afresh.
func (s *Service) blockSubmit(ctx context.Context, chatID ChatID) bool {
	if s.auth == nil {
		return false
	}
	notice, blocked := s.auth.BlockedNotice()
	if !blocked {
		// Clear EVERY chat, not just this one: authorization is process-wide, so one
		// chat submitting is proof the outage is over for all of them. Clearing only
		// the submitting chat would leave a chat that stayed quiet through the
		// recovery window silently un-notified when the next lapse hit it.
		s.mu.Lock()
		clear(s.authNotified)
		s.mu.Unlock()
		return false
	}

	s.log.Warn("provider is not authorized — refusing run", "chat_id", chatID)

	s.mu.Lock()
	first := !s.authNotified[chatID]
	s.authNotified[chatID] = true
	s.mu.Unlock()

	if first && notice != "" {
		// Via notify, which bounds the send: this runs on the CALLER's goroutine, and
		// three of the six callers are loops that must not be stalled — the PR poller
		// dispatches inline, and the restart replay walks every stored marker. A
		// transport that hangs or sits in a 429 back-off would otherwise stop them
		// for good. The refusal itself is already logged at Warn above, so notify's
		// Debug-level delivery failure is enough.
		s.notify(ctx, chatID, notice)
	}
	return true
}
