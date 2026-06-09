//nolint:testpackage // intentionally whitebox to test unexported telegram guard internals
package telegram

import (
	"testing"
	"time"

	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/ratelimit"
)

// fixedNow returns a clock func pinned to t for the rate-limit window.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newCostStore(t *testing.T) *cost.Store {
	t.Helper()
	s, err := cost.Open(t.TempDir() + "/costs.json")
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	return s
}

func TestCheckGuardsAllow(t *testing.T) {
	rl := ratelimit.New(5, time.Minute)
	costs := newCostStore(t)
	allow, reason := CheckGuards(rl, costs, GuardConfig{CostCapUSD: 10, Now: fixedNow(time.Unix(1000, 0))}, 1)
	if !allow || reason != "" {
		t.Fatalf("expected allow, got allow=%v reason=%q", allow, reason)
	}
}

func TestCheckGuardsRateDenied(t *testing.T) {
	rl := ratelimit.New(1, time.Minute)
	costs := newCostStore(t)
	now := fixedNow(time.Unix(2000, 0))
	cfg := GuardConfig{CostCapUSD: 10, Now: now}

	// First request consumes the budget.
	if allow, _ := CheckGuards(rl, costs, cfg, 7); !allow {
		t.Fatal("first request should be allowed")
	}
	// Second within the window is rate-denied with the rate reason.
	allow, reason := CheckGuards(rl, costs, cfg, 7)
	if allow {
		t.Fatal("second request should be rate-denied")
	}
	if reason != rateLimitDenial {
		t.Fatalf("reason = %q, want rate-limit denial", reason)
	}
}

// TestCheckGuardsRateCheckedBeforeCost asserts that when BOTH the rate limit and
// the cost cap would deny, the returned reason is the rate-limit one (rate is
// evaluated first) and the rate budget is what gets consumed.
func TestCheckGuardsRateCheckedBeforeCost(t *testing.T) {
	rl := ratelimit.New(1, time.Minute)
	costs := newCostStore(t)
	const user int64 = 3
	// Push the user over the cost cap so cost would also deny.
	if err := costs.Add(user, 5); err != nil {
		t.Fatalf("seed cost: %v", err)
	}
	cfg := GuardConfig{CostCapUSD: 1, Now: fixedNow(time.Unix(3000, 0))}

	// First call: rate allows but cost denies -> cost reason.
	allow, reason := CheckGuards(rl, costs, cfg, user)
	if allow || reason != costCapDenial {
		t.Fatalf("first call: allow=%v reason=%q, want cost denial", allow, reason)
	}
	// Because the first call was cost-denied (rate allowed and counted it), the
	// single-request rate budget is now spent. A second call is rate-denied, and
	// the rate reason wins because rate is checked before cost.
	allow, reason = CheckGuards(rl, costs, cfg, user)
	if allow || reason != rateLimitDenial {
		t.Fatalf("second call: allow=%v reason=%q, want rate denial (checked first)", allow, reason)
	}
}

func TestCheckGuardsCostDenied(t *testing.T) {
	rl := ratelimit.New(100, time.Minute)
	costs := newCostStore(t)
	const user int64 = 9
	if err := costs.Add(user, 2); err != nil {
		t.Fatalf("seed cost: %v", err)
	}
	allow, reason := CheckGuards(rl, costs, GuardConfig{CostCapUSD: 1, Now: fixedNow(time.Unix(4000, 0))}, user)
	if allow {
		t.Fatal("user over the cap should be denied")
	}
	if reason != costCapDenial {
		t.Fatalf("reason = %q, want cost-cap denial", reason)
	}
}

// TestCheckGuardsDisabled confirms nil limiter and nil store (both disabled) let
// everything through.
func TestCheckGuardsDisabled(t *testing.T) {
	var rl *ratelimit.Limiter
	var costs *cost.Store
	allow, reason := CheckGuards(rl, costs, GuardConfig{CostCapUSD: 0}, 1)
	if !allow || reason != "" {
		t.Fatalf("disabled guards should allow, got allow=%v reason=%q", allow, reason)
	}
}
