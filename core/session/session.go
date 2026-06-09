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
	"strconv"
	"sync"
)

// Store persists chatID -> sessionID durably.
type Store interface {
	// Get returns the stored session id for chatID, with ok=false when none is
	// stored.
	Get(chatID int64) (sessionID string, ok bool)
	// Set stores sessionID for chatID and persists the change durably before
	// returning. An empty sessionID is ignored (there is nothing to resume), so
	// callers can call Set unconditionally with whatever the run captured.
	Set(chatID int64, sessionID string) error
	// Delete removes any stored session id for chatID and persists the change.
	// Provided for completeness (e.g. a future /new reset); the timeout path must
	// NOT call it.
	Delete(chatID int64) error
}

// FileStore is a JSON-file-backed Store. The whole map is held in memory and the
// file is rewritten atomically on every mutation. It is safe for concurrent use.
type FileStore struct {
	path string

	mu       sync.Mutex
	sessions map[int64]string
}

// Open loads (or creates) a FileStore at path. A missing file yields an empty
// store; a present file is parsed as the persisted map. The parent directory is
// created if needed. Reopening the SAME path returns whatever was last
// persisted, which is how sessions survive a process restart.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("session: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("session: create store dir: %w", err)
	}
	s := &FileStore{path: path, sessions: map[int64]string{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent file is not an error
// (a fresh store). The on-disk format is a JSON object keyed by the chat id as a
// string (JSON object keys must be strings), e.g. {"100":"sess-abc"}.
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
		id, convErr := strconv.ParseInt(k, 10, 64)
		if convErr != nil {
			// Skip a malformed key rather than failing the whole load; the store is
			// best-effort continuity, not authoritative data.
			continue
		}
		s.sessions[id] = v
	}
	return nil
}

// Get returns the stored session id for chatID.
func (s *FileStore) Get(chatID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.sessions[chatID]
	return id, ok
}

// Set stores sessionID for chatID and rewrites the file atomically. An empty
// sessionID is a no-op so callers can pass whatever a run captured without
// guarding for the not-yet-known case (and so a failed run never blanks a good
// stored id).
func (s *FileStore) Set(chatID int64, sessionID string) error {
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
func (s *FileStore) Delete(chatID int64) error {
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
	raw := make(map[string]string, len(s.sessions))
	for id, sess := range s.sessions {
		raw[strconv.FormatInt(id, 10)] = sess
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("session: encode store: %w", err)
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("session: create temp store: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename; after a successful rename
	// the temp name no longer exists so the Remove is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: write temp store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: sync temp store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("session: rename store into place: %w", err)
	}
	return nil
}
