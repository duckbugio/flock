package ciwatch

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

// maxStateEntries bounds the dedupe map; when exceeded the whole map is reset
// (the worst case after a reset is one duplicate fix-up per live branch, which
// is harmless — the store is dedupe, not authoritative data).
const maxStateEntries = 500

// State is a small durable dedupe map: "<repo>#<branch>" -> "<sha>#<outcome>"
// of the last handled observation, shaped like the other JSON file stores. A
// nil *State disables dedupe: Handled always reports false and Mark does
// nothing (every scan would re-emit, acceptable for tests).
type State struct {
	path string

	mu      sync.Mutex
	handled map[string]string
}

// OpenState loads (or creates) a State store at path.
func OpenState(path string) (*State, error) {
	if path == "" {
		return nil, errors.New("ciwatch: state path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("ciwatch: create state dir: %w", err)
	}
	s := &State{path: path, handled: map[string]string{}}
	data, err := os.ReadFile(path) //nolint:gosec // operator-configured store path
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("ciwatch: read state: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.handled); err != nil {
		return nil, fmt.Errorf("ciwatch: parse state %s: %w", path, err)
	}
	return s, nil
}

// Handled reports whether key's last handled mark equals mark.
func (s *State) Handled(key, mark string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handled[key] == mark
}

// Mark records mark as key's last handled observation and persists atomically.
func (s *State) Mark(key, mark string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.handled) >= maxStateEntries {
		s.handled = map[string]string{}
	}
	s.handled[key] = mark
	return s.persistLocked()
}

// Prune drops every entry whose key is not in live — the branches currently
// checked out somewhere. Called once per scan so the map tracks live branches
// (a deleted/switched-away branch's entry has no future observation to dedupe)
// instead of growing toward the wholesale-wipe backstop, whose reset would
// re-emit one duplicate fix-up per still-red branch. A no-op (nothing persisted)
// when nothing is stale; nil-safe.
func (s *State) Prune(live map[string]struct{}) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.handled {
		if _, ok := live[k]; !ok {
			delete(s.handled, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

// persistLocked writes the current map atomically. The caller must hold s.mu.
func (s *State) persistLocked() error {
	data, err := json.Marshal(s.handled)
	if err != nil {
		return fmt.Errorf("ciwatch: encode state: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".ciwatch-*.tmp"); err != nil {
		return fmt.Errorf("ciwatch: %w", err)
	}
	return nil
}
