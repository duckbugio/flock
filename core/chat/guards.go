package chat

import (
	"time"

	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/ratelimit"
)

// Denial messages returned by CheckGuards. Engineering artifacts: professional
// English, no duck flavor.
const (
	rateLimitDenial = "Rate limit reached. Try again in a moment."
	costCapDenial   = "Usage cost limit reached."
)

// GuardConfig carries the (value) guardrail tuning CheckGuards needs: the cost
// cap in USD and the clock. The rate-limit budget/window live inside the
// *ratelimit.Limiter itself, so only the cost cap is threaded here.
type GuardConfig struct {
	// CostCapUSD is the cumulative per-user cap. A non-positive value disables the
	// cost gate (Store.Allowed treats it as always-allowed).
	CostCapUSD float64
	// Now supplies the current time for the rate-limit window. Defaults to
	// time.Now when nil, so production callers need not set it; tests inject a
	// fixed clock.
	Now func() time.Time
}

// CheckGuards is the pure, transport-agnostic guardrail decision for one inbound
// request from userID. It is checked AFTER the mention-gate accepts a message and
// BEFORE any paid work (voice transcription, the Claude run), so denied or
// ignored messages spend no limiter budget and never pay for a transcript we
// would discard.
//
// The rate limit is checked FIRST (cheap, in-memory) and, only if it allows,
// the cumulative cost cap (a map lookup). On denial it returns allow=false plus
// a short professional reason for the user; on success allow=true and an empty
// reason. A nil limiter and/or nil cost store mean those guards are disabled
// (always allow), so a partially-configured deploy still works.
//
// CheckGuards has a side effect on the rate limiter: a permitted request is
// counted against the user's window (that is the limiter's whole job). The cost
// gate is read-only here — accumulation happens after a run completes.
func CheckGuards(rl *ratelimit.Limiter, costs *cost.Store, cfg GuardConfig, userID int64) (allow bool, reason string) {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	// Rate limit first: it is the cheapest check and counts the request only when
	// it is allowed, so a cost-denied request still consumes no rate budget.
	if !rl.Allow(userID, now()) {
		return false, rateLimitDenial
	}
	if !costs.Allowed(userID, cfg.CostCapUSD) {
		return false, costCapDenial
	}
	return true, ""
}
