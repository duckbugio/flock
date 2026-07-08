// Package autobudget persists a small, durable per-chat per-day accumulator of
// the USD cost spent by AUTONOMOUS runs — runs no human triggered: PR-poller
// relays, CI-watch fix-ups, scheduled tasks, goal-evaluator rounds and their
// injected follow-ups. It backs the autonomy spending cap: user-triggered runs
// are bounded by the per-user cost cap (core/cost), but an autonomous loop could
// burn budget overnight with nobody watching, so it gets its own, much smaller,
// per-day ceiling.
//
// The implementation mirrors core/cost.Store: a single JSON file, loaded once on
// Open and rewritten atomically on each mutation, guarded by a mutex. Keys are
// "<chatID>#<YYYY-MM-DD>" (UTC day), and days older than the retention window are
// pruned on write so the file never grows unbounded. A nil *Store is a valid
// no-op store (auto budget disabled): Allowed always returns true and Add does
// nothing.
package autobudget

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/duckbugio/flock/internal/atomicfile"
)

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// dayFormat renders the UTC day segment of a store key.
const dayFormat = "2006-01-02"

// retainDays is how many trailing days of accounting are kept on write. Two days
// covers the current day plus the previous one across any timezone skew; older
// entries only matter for a cap that resets daily, so they are pruned.
const retainDays = 2

// Store persists chatID+day -> accumulated USD durably. The whole map is held in
// memory and the file is rewritten atomically on every Add. It is safe for
// concurrent use. A nil *Store is a valid no-op store (auto budget disabled).
type Store struct {
	path string

	mu     sync.Mutex
	totals map[string]float64
}

// Open loads (or creates) a Store at path. A missing file yields an empty store;
// a present file is parsed as the persisted map. The parent directory is created
// if needed. Reopening the SAME path returns whatever was last persisted, which
// is how the daily totals survive a process restart.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("autobudget: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("autobudget: create store dir: %w", err)
	}
	s := &Store{path: path, totals: map[string]float64{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent or empty file is not
// an error (a fresh store). The on-disk format is a JSON object keyed by
// "<chatID>#<YYYY-MM-DD>", e.g. {"100#2026-07-08": 1.25}.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("autobudget: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &s.totals); err != nil {
		return fmt.Errorf("autobudget: parse store %s: %w", s.path, err)
	}
	return nil
}

// key builds the per-chat per-UTC-day accumulator key.
func key(chatID string, now time.Time) string {
	return chatID + "#" + now.UTC().Format(dayFormat)
}

// SpentToday returns the USD already spent by autonomous runs in chatID during
// now's UTC day. A nil store reports zero.
func (s *Store) SpentToday(chatID string, now time.Time) float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals[key(chatID, now)]
}

// Allowed reports whether chatID is still under the per-day autonomy cap. It
// returns false once the day's accumulated total is >= capUSD. A non-positive
// capUSD disables the cap (always allowed), and a nil store is likewise always
// allowed. Like the per-user cost cap, this is REACTIVE: cost is known only
// after a run completes, so the crossing run still ran.
func (s *Store) Allowed(chatID string, now time.Time, capUSD float64) bool {
	if s == nil || capUSD <= 0 {
		return true
	}
	return s.SpentToday(chatID, now) < capUSD
}

// Add accumulates usd onto chatID's total for now's UTC day and rewrites the
// file atomically, pruning entries older than the retention window. A
// non-positive usd is a no-op so callers can Add unconditionally with whatever a
// run captured. A nil store is also a no-op.
func (s *Store) Add(chatID string, now time.Time, usd float64) error {
	if s == nil || usd <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals[key(chatID, now)] += usd
	s.pruneLocked(now)
	return s.persistLocked()
}

// pruneLocked drops accumulator entries whose day is older than the retention
// window, keeping the file small. Malformed keys are dropped too (the store is
// best-effort accounting, not authoritative data). The caller must hold s.mu.
func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.UTC().AddDate(0, 0, -retainDays).Format(dayFormat)
	for k := range s.totals {
		i := strings.LastIndexByte(k, '#')
		if i < 0 || k[i+1:] < cutoff {
			delete(s.totals, k)
		}
	}
}

// persistLocked writes the current map to a temp file in the same directory and
// renames it over the target, so a crash mid-write never leaves a half-written
// or corrupt store. The caller must hold s.mu.
func (s *Store) persistLocked() error {
	data, err := json.Marshal(s.totals)
	if err != nil {
		return fmt.Errorf("autobudget: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".autobudget-*.tmp"); err != nil {
		return fmt.Errorf("autobudget: %w", err)
	}
	return nil
}
