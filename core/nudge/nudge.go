// Package nudge persists a single, durable "starred resolved" flag for the
// deployment's GitHub account so the post-task star nudge stops once the repo is
// known to be starred (whether observed via the API or set by a successful
// button press). A deployment authenticates as ONE account, so this is a single
// boolean, not a per-chat map. It is shaped like core/session.FileStore: a tiny
// JSON file rewritten atomically (temp file + rename) under a mutex. A missing
// file means "not yet starred".
package nudge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/duckbugio/flock/internal/atomicfile"
)

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// state is the on-disk JSON shape, e.g. {"starred":true}.
type state struct {
	Starred bool `json:"starred"`
}

// Store persists the global "starred resolved" flag durably. It is safe for
// concurrent use.
type Store struct {
	path string

	mu      sync.Mutex
	starred bool
}

// Open loads (or creates) a Store at path. A missing file yields an unstarred
// store; a present file is parsed for the flag. The parent directory is created
// if needed. Reopening the SAME path reflects whatever was last persisted, which
// is how the resolved state survives a process restart.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("nudge: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("nudge: create store dir: %w", err)
	}
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent or empty file is not
// an error (a fresh, unstarred store).
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("nudge: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("nudge: parse store %s: %w", s.path, err)
	}
	s.starred = st.Starred
	return nil
}

// Starred reports whether the repo has been resolved as starred. A nil store is
// treated as "not starred" so callers can stay nil-safe.
func (s *Store) Starred() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starred
}

// MarkStarred records that the repo is starred and persists the change
// atomically. It is idempotent: marking an already-starred store is a no-op that
// avoids a needless rewrite. A nil store is a no-op.
func (s *Store) MarkStarred() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.starred {
		return nil
	}
	s.starred = true
	return s.persistLocked()
}

// persistLocked writes the state to a temp file in the same directory and
// renames it over the target, so a crash mid-write never leaves a half-written
// store (rename is atomic on the same filesystem). The caller must hold s.mu.
func (s *Store) persistLocked() error {
	data, err := json.Marshal(state{Starred: s.starred})
	if err != nil {
		return fmt.Errorf("nudge: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".nudge-*.tmp"); err != nil {
		return fmt.Errorf("nudge: %w", err)
	}
	return nil
}
