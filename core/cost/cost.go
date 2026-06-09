// Package cost persists a small, durable per-user accumulator of Claude run cost
// in USD so the inbound message path can enforce a cumulative per-user cost cap
// across messages and process restarts. It is shaped exactly like
// core/session.FileStore: a single JSON file, loaded once on Open and rewritten
// atomically (temp file + rename) on each mutation, guarded by a mutex so the
// per-update goroutines are race-safe. JSON keeps it dependency-free and
// trivially correct for a small map.
//
// The cap is REACTIVE: cost is only known after a run completes, so Allowed
// denies the NEXT request once the accumulated total has crossed the cap — the
// request that crossed it still ran. That is expected and accepted (the cap
// bounds total spend within a bounded overshoot of one request).
package cost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/duckbugio/flock/internal/atomicfile"
)

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// Store persists userID -> accumulated USD durably. The whole map is held in
// memory and the file is rewritten atomically on every Add. It is safe for
// concurrent use. A nil *Store is a valid no-op store (cost tracking disabled):
// Allowed always returns true and Add does nothing, so callers that failed to
// open a store can still wire it without nil checks.
type Store struct {
	path string

	mu     sync.Mutex
	totals map[int64]float64
}

// Open loads (or creates) a Store at path. A missing file yields an empty store;
// a present file is parsed as the persisted map. The parent directory is created
// if needed. Reopening the SAME path returns whatever was last persisted, which
// is how accumulated totals survive a process restart.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("cost: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("cost: create store dir: %w", err)
	}
	s := &Store{path: path, totals: map[int64]float64{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent file is not an error
// (a fresh store). The on-disk format is a JSON object keyed by the user id as a
// string (JSON object keys must be strings), e.g. {"100":1.25}.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cost: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	raw := map[string]float64{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("cost: parse store %s: %w", s.path, err)
	}
	for k, v := range raw {
		id, convErr := strconv.ParseInt(k, 10, 64)
		if convErr != nil {
			// Skip a malformed key rather than failing the whole load; the store is
			// best-effort accounting, not authoritative data.
			continue
		}
		s.totals[id] = v
	}
	return nil
}

// Allowed reports whether userID is still under the cumulative cost cap. It
// returns false once the user's accumulated total is >= capUSD. A non-positive
// capUSD disables the cap (always allowed), and a nil store is likewise always
// allowed. Allowed is safe for concurrent use.
func (s *Store) Allowed(userID int64, capUSD float64) bool {
	if s == nil || capUSD <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals[userID] < capUSD
}

// Add accumulates usd onto userID's running total and rewrites the file
// atomically. A non-positive usd (e.g. a failed/stopped run that produced no
// Result) is a no-op so callers can Add unconditionally with whatever a run
// captured. A nil store is also a no-op. Add is safe for concurrent use.
func (s *Store) Add(userID int64, usd float64) error {
	if s == nil || usd <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals[userID] += usd
	return s.persistLocked()
}

// persistLocked writes the current map to a temp file in the same directory and
// renames it over the target, so a crash mid-write never leaves a half-written
// or corrupt store (rename is atomic on the same filesystem). The caller must
// hold s.mu.
func (s *Store) persistLocked() error {
	raw := make(map[string]float64, len(s.totals))
	for id, total := range s.totals {
		raw[strconv.FormatInt(id, 10)] = total
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("cost: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".costs-*.tmp"); err != nil {
		return fmt.Errorf("cost: %w", err)
	}
	return nil
}
