// Package pending persists a small, durable per-chat QUEUE of "interrupted run"
// markers so a run that is still in-flight (or merely queued) when the process is
// killed (e.g. a SIGKILL after the graceful-drain window during a deploy) can be
// auto-resumed on the next startup WITHOUT the user re-sending their message.
//
// A marker records a process-unique ID, the prompt, the dangling "Working…"
// anchor message id, and the run's start time. A chat's dispatch lane is serial,
// so at deploy time a chat can hold one RUNNING run plus several QUEUED runs; a
// single per-chat marker could capture only one of them and a queued job never
// reaches run-start to write its own. The store therefore keeps a FIFO QUEUE of
// markers per chat, and a marker is enqueued at SUBMIT time (not run start) so a
// queued-but-never-started job is captured too.
//
// Markers are cleared BY ID on every clean terminal (normal Result, RunError,
// timeout, spawn error) so a stale or cancelled run can never remove a newer
// queued run's marker, and a whole lane is cleared at once when the user
// intentionally purges it (Stop / edit-supersede, where there is no resubmit to
// preserve). A run cancelled by the dispatcher shutting the process down keeps
// its marker so the next startup auto-resumes it.
//
// The implementation mirrors core/session.FileStore: a single JSON file, loaded
// once on Open and rewritten atomically (temp file + rename) on each mutation,
// guarded by a mutex so concurrent chats are race-safe. The on-disk format is a
// JSON object keyed by chat id whose value is the ordered slice of that chat's
// markers, e.g. {"100":[{"id":"1",…},{"id":"2",…}]}. IDs come from a monotonic
// counter seeded on load above the max persisted id, so an Enqueue after a
// restart can never collide with a persisted marker the resume loop reuses.
package pending

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

// Marker is one interrupted-run record. ID is a process-unique decimal string
// assigned at Enqueue (used to clear the exact entry on a terminal). AnchorMsgID
// may be empty when the anchor message could not be created; StartedAt is the
// Unix-millisecond start time, carried for diagnostics and future staleness
// pruning.
type Marker struct {
	ID          string `json:"id"`
	Prompt      string `json:"prompt"`
	AnchorMsgID string `json:"anchorMsgId"`
	StartedAt   int64  `json:"startedAt"`
}

// Store persists a per-chat FIFO queue of markers durably and is safe for
// concurrent use. The chat id is the transport's opaque string id (see
// core/chat.ChatID).
type Store interface {
	// Enqueue assigns a fresh process-unique ID to m, appends it to chatID's
	// queue, persists the change durably, and returns the assigned ID. m is
	// passed in WITHOUT an ID. Called at submit time so a queued-but-never-started
	// run is captured.
	Enqueue(chatID string, m Marker) (string, error)
	// SetAnchor sets the AnchorMsgID of the marker with the given id in chatID's
	// queue and persists the change. A missing chat/id is a harmless no-op (the
	// marker may already have been cleared).
	SetAnchor(chatID, id, anchorMsgID string) error
	// Remove drops the marker with the given id from chatID's queue and persists
	// the change; if the queue becomes empty the chat key is removed. Removing an
	// absent chat/id is a harmless no-op (idempotent). Called by id on a clean
	// terminal so a stale run never removes a newer queued run's marker.
	Remove(chatID, id string) error
	// Clear removes ALL markers for chatID (the whole lane) and persists the
	// change. Clearing an absent chat is a harmless no-op. Called when the user
	// intentionally purges the lane (Stop / edit-supersede).
	Clear(chatID string) error
	// All returns a copy of every chat's ordered marker slice, for the startup
	// replay loop.
	All() map[string][]Marker
}

// FileStore is a JSON-file-backed Store. The whole map is held in memory and the
// file is rewritten atomically on every mutation. It is safe for concurrent use.
type FileStore struct {
	path string

	mu      sync.Mutex
	markers map[string][]Marker
	nextID  uint64 // monotonic ID counter, mutated only under mu
}

// Open loads (or creates) a FileStore at path. A missing file yields an empty
// store; a present file is parsed as the persisted per-chat queue map. The parent
// directory is created if needed. Reopening the SAME path returns whatever was
// last persisted, which is how interrupted-run markers survive a process restart.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("pending: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("pending: create store dir: %w", err)
	}
	s := &FileStore{path: path, markers: map[string][]Marker{}, nextID: 1}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent or empty file is not an
// error (a fresh store). The on-disk format is a JSON object keyed by chat id
// whose value is that chat's ordered marker slice. After loading, the ID counter
// is seeded to max(parsed numeric id) + 1 so a NEW Enqueue after a restart can
// never collide with the id of a persisted-but-not-yet-cleared marker that the
// resume loop reuses.
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
	raw := map[string][]Marker{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("pending: parse store %s: %w", s.path, err)
	}
	var maxID uint64
	for k, v := range raw {
		s.markers[k] = v
		for _, m := range v {
			// Unparseable ids are ignored for the max (defensive; ids are always
			// assigned numerically by Enqueue).
			if n, perr := strconv.ParseUint(m.ID, 10, 64); perr == nil && n > maxID {
				maxID = n
			}
		}
	}
	s.nextID = maxID + 1
	return nil
}

// Enqueue assigns a fresh ID to m, appends it to chatID's queue, persists the
// change atomically, and returns the assigned ID. A nil store is a no-op that
// returns "" so callers stay nil-safe when the store failed to open.
func (s *FileStore) Enqueue(chatID string, m Marker) (string, error) {
	if s == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strconv.FormatUint(s.nextID, 10)
	s.nextID++
	m.ID = id
	s.markers[chatID] = append(s.markers[chatID], m)
	if err := s.persistLocked(); err != nil {
		return "", err
	}
	return id, nil
}

// SetAnchor sets the AnchorMsgID of the marker with id in chatID's queue and
// rewrites the file. A missing chat/id is a no-op (no needless rewrite); the
// marker may already have been cleared. A nil store is a no-op.
func (s *FileStore) SetAnchor(chatID, id, anchorMsgID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.markers[chatID] {
		if s.markers[chatID][i].ID == id {
			s.markers[chatID][i].AnchorMsgID = anchorMsgID
			return s.persistLocked()
		}
	}
	return nil
}

// Remove drops the marker with id from chatID's queue and rewrites the file; if
// the queue becomes empty the chat key is removed. Removing an absent chat/id is
// a no-op (idempotent). A nil store is a no-op.
func (s *FileStore) Remove(chatID, id string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.markers[chatID]
	if !ok {
		return nil
	}
	idx := -1
	for i := range q {
		if q[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	q = append(q[:idx], q[idx+1:]...)
	if len(q) == 0 {
		delete(s.markers, chatID)
	} else {
		s.markers[chatID] = q
	}
	return s.persistLocked()
}

// Clear removes ALL markers for chatID (the whole lane) and rewrites the file.
// Clearing an absent chat is a no-op (no needless rewrite). A nil store is a
// no-op.
func (s *FileStore) Clear(chatID string) error {
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

// All returns a copy of every chat's ordered marker slice. A nil store yields an
// empty map so the startup replay loop stays nil-safe.
func (s *FileStore) All() map[string][]Marker {
	out := map[string][]Marker{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.markers {
		out[k] = append([]Marker(nil), v...)
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
