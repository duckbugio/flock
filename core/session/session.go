// Package session persists a small, durable map of chatID -> Claude session_id
// so the adapter can resume each chat's context across messages (and across
// process restarts) via the CLI's --resume flag. The store is the §4 "session
// continuity" mechanism: a timeout/cancel must KEEP the stored id (not discard
// it), so the next message resumes seamlessly — that is the §7.3 wart fix.
//
// The implementation is a single JSON file, loaded once on Open and rewritten
// atomically (temp file + rename) on each Set, guarded by a mutex so concurrent
// chats are race-safe. JSON keeps it dependency-free and trivially correct for a
// small map; sqlite would be overkill here.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/duckbugio/flock/internal/atomicfile"
)

// Store persists chatID -> sessionID durably. The chat id is the transport's
// opaque string id (see core/chat.ChatID); for Telegram it is the numeric chat
// id rendered as a string, so the on-disk format is unchanged.
type Store interface {
	// Get returns the stored session id for chatID, with ok=false when none is
	// stored.
	Get(chatID string) (sessionID string, ok bool)
	// Set stores sessionID for chatID and persists the change durably before
	// returning. An empty sessionID is ignored (there is nothing to resume), so
	// callers can call Set unconditionally with whatever the run captured.
	Set(chatID, sessionID string) error
	// Delete removes any stored session id for chatID and persists the change.
	// Provided for completeness (e.g. a future /new reset); the timeout path must
	// NOT call it.
	Delete(chatID string) error
}

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// FileStore is a JSON-file-backed Store. The whole map is held in memory and the
// file is rewritten atomically on every mutation. It is safe for concurrent use.
type FileStore struct {
	path string

	mu       sync.Mutex
	sessions map[string]string
}

// Open loads (or creates) a FileStore at path. A missing file yields an empty
// store; a present file is parsed as the persisted map. The parent directory is
// created if needed. Reopening the SAME path returns whatever was last
// persisted, which is how sessions survive a process restart.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("session: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("session: create store dir: %w", err)
	}
	s := &FileStore{path: path, sessions: map[string]string{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent file is not an error
// (a fresh store). The on-disk format is a JSON object keyed by the chat id as a
// string (JSON object keys are strings), e.g. {"100":"sess-abc"}.
func (s *FileStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	raw := map[string]string{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("session: parse store %s: %w", s.path, err)
	}
	for k, v := range raw {
		s.sessions[k] = v
	}
	return nil
}

// Get returns the stored session id for chatID.
func (s *FileStore) Get(chatID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.sessions[chatID]
	return id, ok
}

// Set stores sessionID for chatID and rewrites the file atomically. An empty
// sessionID is a no-op so callers can pass whatever a run captured without
// guarding for the not-yet-known case (and so a failed run never blanks a good
// stored id).
func (s *FileStore) Set(chatID, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[chatID] == sessionID {
		return nil // unchanged; avoid a needless rewrite
	}
	s.sessions[chatID] = sessionID
	return s.persistLocked()
}

// Delete removes chatID's stored session id and rewrites the file. Deleting an
// absent chat is a no-op.
func (s *FileStore) Delete(chatID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[chatID]; !ok {
		return nil
	}
	delete(s.sessions, chatID)
	return s.persistLocked()
}

// persistLocked writes the current map to a temp file in the same directory and
// renames it over the target, so a crash mid-write never leaves a half-written
// or corrupt store (rename is atomic on the same filesystem). The caller must
// hold s.mu.
func (s *FileStore) persistLocked() error {
	data, err := json.Marshal(s.sessions)
	if err != nil {
		return fmt.Errorf("session: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".sessions-*.tmp"); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}
