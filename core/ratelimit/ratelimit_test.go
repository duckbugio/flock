//nolint:testpackage // intentionally whitebox to test unexported ratelimit internals
package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllowFixedWindowBoundary(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	tests := []struct {
		name string
		max  int
		win  time.Duration
		// calls is a sequence of (offset-from-base, wantAllow) for a single user.
		calls []struct {
			at   time.Duration
			want bool
		}
	}{
		{
			name: "Nth allowed, N+1 denied within window",
			max:  3,
			win:  time.Minute,
			calls: []struct {
				at   time.Duration
				want bool
			}{
				{0, true},                // 1
				{1 * time.Second, true},  // 2
				{2 * time.Second, true},  // 3 (== max, still allowed)
				{3 * time.Second, false}, // 4th in window denied
				{4 * time.Second, false}, // still denied within the same window
			},
		},
		{
			name: "capacity restores after the window elapses",
			max:  2,
			win:  time.Minute,
			calls: []struct {
				at   time.Duration
				want bool
			}{
				{0, true},                // 1
				{1 * time.Second, true},  // 2
				{2 * time.Second, false}, // 3rd denied
				{time.Minute, true},      // window elapsed -> fresh window, allowed
				{time.Minute + 1, true},  // 2nd of new window
				{time.Minute + 2, false}, // 3rd of new window denied
			},
		},
		{
			name: "disabled when max <= 0",
			max:  0,
			win:  time.Minute,
			calls: []struct {
				at   time.Duration
				want bool
			}{
				{0, true}, {0, true}, {0, true},
			},
		},
		{
			name: "disabled when window <= 0",
			max:  3,
			win:  0,
			calls: []struct {
				at   time.Duration
				want bool
			}{
				{0, true}, {0, true}, {0, true}, {0, true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := New(tt.max, tt.win)
			const user int64 = 42
			for i, c := range tt.calls {
				if got := l.Allow(user, base.Add(c.at)); got != c.want {
					t.Errorf("call %d at +%v: Allow = %v, want %v", i, c.at, got, c.want)
				}
			}
		})
	}
}

// TestAllowPerUserIndependent confirms one user's exhausted budget does not deny
// another user.
func TestAllowPerUserIndependent(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	l := New(1, time.Minute)
	if !l.Allow(1, base) {
		t.Fatal("user 1 first call should be allowed")
	}
	if l.Allow(1, base) {
		t.Fatal("user 1 second call should be denied")
	}
	if !l.Allow(2, base) {
		t.Fatal("user 2 first call should be allowed despite user 1 being capped")
	}
}

// TestAllowNilLimiterAllows confirms a nil *Limiter is a permissive no-op.
func TestAllowNilLimiterAllows(t *testing.T) {
	var l *Limiter
	if !l.Allow(1, time.Now()) {
		t.Fatal("nil limiter must always allow")
	}
}

// TestAllowConcurrent exercises many goroutines hitting the same user under the
// race detector and asserts exactly max requests are admitted within one window.
func TestAllowConcurrent(t *testing.T) {
	const (
		maxReqs = 50
		workers = 500
	)
	l := New(maxReqs, time.Minute)
	now := time.Unix(3_000_000, 0)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if l.Allow(7, now) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != maxReqs {
		t.Fatalf("admitted %d requests within one window, want exactly %d", got, maxReqs)
	}
}
