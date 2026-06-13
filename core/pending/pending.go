// Package pending persists a small, durable map of chatID -> "interrupted run"
// marker so a run that is still in-flight when the process is killed (e.g. a
// SIGKILL after the graceful-drain window during a deploy) can be auto-resumed
// on the next startup WITHOUT the user re-sending their message.
//
// A marker records the prompt, the dangling "Working…" anchor message id, and
// the run's start time. It is written at run START (as early as possible, before
// the run can be torn down) and cleared on EVERY clean terminal (normal Result,
// RunError, timeout, user Stop / edit-supersede) so that a deliberately stopped
// or finished run is NOT resumed. The run-start write is the only writer, so a
// finished run is never re-marked.
//
// The implementation mirrors core/session.FileStore: a single JSON file, loaded
// once on Open and rewritten atomically (temp file + rename) on each mutation,
// guarded by a mutex so concurrent chats are race-safe. The value is struct-
// shaped (see core/nudge for the struct-valued variant). JSON keeps it
// dependency-free and trivially correct for a small map.
package pending

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

// Marker is one chat's interrupted-run record. AnchorMsgID may be empty when the
// anchor message could not be created; StartedAt is the Unix-millisecond start
// time, carried for diagnostics and future staleness pruning.
type Marker struct {
	Prompt      string `json:"prompt"`
	AnchorMsgID string `json:"anchorMsgId"`
	StartedAt   int64  `json:"startedAt"`
}

// Store persists chatID -> Marker durably and is safe for concurrent use. The
// chat id is the transport's opaque string id (see core/chat.ChatID).
type Store interface {
	// Set records marker for chatID and persists the change durably before
	// returning. Called once at run start.
	Set(chatID string, marker Marker) error
	// Delete removes any stored marker for chatID and persists the change.
	// Deleting an absent chat is a harmless no-op. Called on every clean terminal.
	Delete(chatID string) error
	// All returns a copy of every stored marker keyed by chat id, for the startup
	// replay loop.
	All() map[string]Marker
}

// FileStore is a JSON-file-backed Store. The whole map is held in memory and the
// file is rewritten atomically on every mutation. It is safe for concurrent use.
type FileStore struct {
	path string

	mu      sync.Mutex
	markers map[string]Marker
}

// Open loads (or creates) a FileStore at path. A missing file yields an empty
// store; a present file is parsed as the persisted map. The parent directory is
// created if needed. Reopening the SAME path returns whatever was last persisted,
// which is how interrupted-run markers survive a process restart.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("pending: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("pending: create store dir: %w", err)
	}
	s := &FileStore{path: path, markers: map[string]Marker{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent or empty file is not an
// error (a fresh store). The on-disk format is a JSON object keyed by the chat id
// as a string, e.g. {"100":{"prompt":"…","anchorMsgId":"42","startedAt":1}}.
func (s *FileStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("pending: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	raw := map[string]Marker{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("pending: parse store %s: %w", s.path, err)
	}
	for k, v := range raw {
		s.markers[k] = v
	}
	return nil
}

// Set records marker for chatID and rewrites the file atomically. A nil store is
// a no-op so callers can stay nil-safe when the store failed to open.
func (s *FileStore) Set(chatID string, marker Marker) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markers[chatID] = marker
	return s.persistLocked()
}

// Delete removes chatID's stored marker and rewrites the file. Deleting an absent
// chat is a no-op (no needless rewrite). A nil store is a no-op.
func (s *FileStore) Delete(chatID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.markers[chatID]; !ok {
		return nil
	}
	delete(s.markers, chatID)
	return s.persistLocked()
}

// All returns a copy of every stored marker. A nil store yields an empty map so
// the startup replay loop stays nil-safe.
func (s *FileStore) All() map[string]Marker {
	out := map[string]Marker{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.markers {
		out[k] = v
	}
	return out
}

// persistLocked writes the current map to a temp file in the same directory and
// renames it over the target, so a crash mid-write never leaves a half-written or
// corrupt store (rename is atomic on the same filesystem). The caller must hold
// s.mu.
func (s *FileStore) persistLocked() error {
	data, err := json.Marshal(s.markers)
	if err != nil {
		return fmt.Errorf("pending: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".pending-*.tmp"); err != nil {
		return fmt.Errorf("pending: %w", err)
	}
	return nil
}
