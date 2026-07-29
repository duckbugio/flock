package chat

import "context"

// RunGate reports whether the configured AI provider can run at all right now.
// It exists for one case today: a Codex subscription deployment that has not
// completed its interactive sign-in yet (core/codexauth.Manager satisfies it).
//
// The gate lives HERE, at the single point every run passes through, rather than
// only in the adapters' message paths. Runs reach the provider from five places —
// a user message, a poller-injected PR comment, a cron fire, a workspace
// follow-up, and the restart replay of an interrupted run — and only the first
// of those goes through an adapter. Gating in the adapters alone would let the
// other four march into a CLI that cannot authenticate, once per schedule tick,
// with nothing user-visible to explain it.
type RunGate interface {
	// BlockedNotice returns the user-facing explanation and true when runs must
	// be blocked, or ("", false) when they may proceed.
	BlockedNotice() (string, bool)
}

// blockRun reports whether this run must be dropped because the provider is
// unauthorized, and tells the chat once why.
//
// The notice is sent at most ONCE per chat per unauthorized period: background
// sources are recurring (a five-minute cron would otherwise post a notice every
// five minutes), and the adapters already answer a user's own message directly.
// The drop is logged every time, so the operator sees the full picture in the
// logs while the chat stays readable. The counter resets as soon as the provider
// is authorized again, so a later lapse is announced afresh.
func (s *Service) blockRun(ctx context.Context, chatID ChatID) bool {
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

	s.log.Warn("provider is not authorized — dropping run", "chat_id", chatID)

	s.mu.Lock()
	first := !s.authNotified[chatID]
	s.authNotified[chatID] = true
	s.mu.Unlock()

	if first && notice != "" {
		if _, err := s.chat.Send(ctx, chatID, notice, "", true); err != nil {
			s.log.Error("send unauthorized notice", "chat_id", chatID, "error", err)
		}
	}
	return true
}
