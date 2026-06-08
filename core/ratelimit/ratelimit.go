// Package ratelimit provides an in-memory, goroutine-safe, fixed-window
// per-user request limiter for the inbound message path. It is deliberately a
// simple fixed-window counter rather than a token-bucket library: the guardrail
// only needs to cap how many requests a single user may make within a rolling
// window, and a counter keyed by user id is the smallest correct tool for that.
//
// The limiter is hit concurrently from the go-telegram per-update goroutines
// (each update is dispatched on its own goroutine), so every access is guarded
// by a single mutex.
package ratelimit

import (
	"sync"
	"time"
)

// window tracks a single user's fixed window: the timestamp the window started
// and how many requests have been counted in it.
type window struct {
	start time.Time
	count int
}

// Limiter caps each user to maxRequests calls within any window-length span. It
// is a fixed-window counter: the first allowed call in a fresh window stamps the
// window start; subsequent calls within window of that start increment the
// counter; the (maxRequests+1)th call in the same window is denied. Once window
// has elapsed since the start, the next call opens a new window and capacity is
// restored.
type Limiter struct {
	maxRequests int
	window      time.Duration

	mu    sync.Mutex
	users map[int64]*window
}

// New builds a Limiter permitting maxRequests calls per user within any window.
// When maxRequests <= 0 or window <= 0 the limiter is disabled and Allow always
// returns true (the feature is off), so callers need not special-case the
// disabled configuration.
func New(maxRequests int, win time.Duration) *Limiter {
	return &Limiter{
		maxRequests: maxRequests,
		window:      win,
		users:       map[int64]*window{},
	}
}

// Allow reports whether userID may make a request at time now, recording the
// request when it is permitted. It permits exactly maxRequests calls within any
// window; the (maxRequests+1)th in-window call is denied. After the window
// elapses, capacity is restored on the next call.
//
// A disabled limiter (maxRequests <= 0 or window <= 0) always allows and records
// nothing. Allow is safe for concurrent use.
func (l *Limiter) Allow(userID int64, now time.Time) bool {
	if l == nil || l.maxRequests <= 0 || l.window <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.users[userID]
	if !ok || now.Sub(w.start) >= l.window {
		// First request, or the previous window has fully elapsed: open a fresh
		// window starting now with this request counted.
		l.users[userID] = &window{start: now, count: 1}
		return true
	}
	if w.count >= l.maxRequests {
		// The window is still open and already at capacity: deny without counting,
		// so a denied request does not extend or inflate the window.
		return false
	}
	w.count++
	return true
}
