package chat

import "context"

// RunGate reports whether the configured AI provider can run at all right now.
// It exists for one case today: a Codex subscription deployment that has not
// completed its interactive sign-in yet (core/codexauth.Manager satisfies it).
//
// The gate lives HERE, at the submit paths every run enters through, rather than
// only in the adapters' message paths. Runs reach the provider from five places —
// a user message, a poller-injected PR comment, a cron fire, a workspace
// follow-up and the restart replay of an interrupted run — and only the first of
// those goes through an adapter. Gating in the adapters alone would let the other
// four march into a CLI that cannot authenticate, once per schedule tick, with
// nothing user-visible to explain it.
type RunGate interface {
	// BlockedNotice returns the user-facing explanation and true when runs must
	// be blocked, or ("", false) when they may proceed.
	BlockedNotice() (string, bool)
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
		s.mu.Lock()
		delete(s.authNotified, chatID)
		s.mu.Unlock()
		return false
	}

	s.log.Warn("provider is not authorized — refusing run", "chat_id", chatID)

	s.mu.Lock()
	first := !s.authNotified[chatID]
	s.authNotified[chatID] = true
	s.mu.Unlock()

	if first && notice != "" {
		// Sent synchronously on the caller's own goroutine, so a request context is
		// still alive where there is one; the background sources pass context
		// Background because they have none.
		if _, err := s.chat.Send(ctx, chatID, notice, "", true); err != nil {
			s.log.Error("send unauthorized notice", "chat_id", chatID, "error", err)
		}
	}
	return true
}
