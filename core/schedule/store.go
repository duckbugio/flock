// Package schedule persists a durable per-chat set of cron JOBS and runs a
// background scheduler that fires each due job into the same dispatch lane a
// normal message uses (via a fire callback wired to chat.Service.Inject). It is
// transport-neutral: a chat id is the transport's opaque string id and a user id
// is a plain int64, so the package imports neither adapter nor core/chat (which
// would create an import cycle).
//
// A Job records a process-unique id, the chat it belongs to, a human name, the
// five-field cron spec, the prompt to inject, who created it, whether it is
// active, when it was created, and the last minute key it fired (for de-dup). The
// store mirrors core/pending.FileStore: a single JSON file keyed by chat id whose
// value is that chat's ordered slice of jobs, loaded once on Open and rewritten
// atomically (temp file + rename) on every mutation under a mutex, so concurrent
// chats are race-safe. IDs come from a monotonic counter seeded above the max
// persisted id on load, so a new Add after a restart never collides with a
// persisted job.
//
// Timezone: cron specs are evaluated in PROCESS-LOCAL time (the Manager passes
// time.Now, which is local; the container defaults to UTC). A job's minute is
// matched in whatever local zone the process runs in.
//
// Day-of-month / day-of-week use AND-semantics (see cron.go): a job that
// restricts BOTH day fields fires only when both match. Restrict at most one.
package schedule

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

// dirPerm is the owner-only permission for bot-created directories (mirrors
// core/pending).
const dirPerm os.FileMode = 0o750

// MaxJobsPerChat caps how many scheduled jobs a single chat may hold. Each active
// job is an UNATTENDED, recurring run (its prompt can drive commits/PRs) that
// fires outside the live per-message rate/cost guards, so an unbounded count is a
// cost/autonomy amplifier. The cap bounds that blast radius while staying well
// above any realistic per-chat schedule.
const MaxJobsPerChat = 25

// ErrTooManyJobs is returned by Add when a chat is already at MaxJobsPerChat, so
// the caller can surface a clear "remove one first" message instead of a generic
// failure.
var ErrTooManyJobs = errors.New("schedule: chat has reached the maximum number of jobs")

// Job is one scheduled cron job. ID is a process-unique decimal string assigned
// at Add. ChatID is the transport's opaque string chat id the job (and its fired
// prompt) belongs to. Name is a human label; Cron is the five-field spec; Prompt
// is injected into the chat when the job fires. CreatedBy is the user id that
// created it (a sentinel for accountability). Active toggles whether the
// scheduler considers it (paused jobs are kept but skipped). CreatedAt is the
// Unix-second creation time. LastFired is the minute key ("2006-01-02T15:04")
// already fired, so a sub-minute scheduler tick fires a matching minute at most
// once.
type Job struct {
	ID        string `json:"id"`
	ChatID    string `json:"chatId"`
	Name      string `json:"name"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	CreatedBy int64  `json:"createdBy"`
	Active    bool   `json:"active"`
	CreatedAt int64  `json:"createdAt"`
	LastFired string `json:"lastFired"`
}

// Store persists a per-chat slice of cron jobs durably and is safe for concurrent
// use. The whole map is held in memory and the backing file is rewritten
// atomically on every mutation.
type Store struct {
	path string

	mu     sync.Mutex
	jobs   map[string][]Job
	nextID uint64 // monotonic ID counter, mutated only under mu
}

// Open loads (or creates) a Store at path. A missing file yields an empty store;
// a present file is parsed as the persisted per-chat job map. The parent
// directory is created if needed. Reopening the SAME path returns whatever was
// last persisted, which is how jobs survive a process restart.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("schedule: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("schedule: create store dir: %w", err)
	}
	s := &Store{path: path, jobs: map[string][]Job{}, nextID: 1}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent or empty file is not an
// error (a fresh store). The ID counter is seeded to max(parsed numeric id) + 1 so
// a new Add after a restart never collides with a persisted job's id.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("schedule: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	raw := map[string][]Job{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("schedule: parse store %s: %w", s.path, err)
	}
	var maxID uint64
	for k, v := range raw {
		s.jobs[k] = v
		for _, j := range v {
			if n, perr := strconv.ParseUint(j.ID, 10, 64); perr == nil && n > maxID {
				maxID = n
			}
		}
	}
	s.nextID = maxID + 1
	return nil
}

// Add assigns a fresh process-unique ID to job, appends it to its chat's slice,
// persists the change atomically, and returns the assigned ID. job is passed in
// WITHOUT an ID; its ChatID must already be set. It returns ErrTooManyJobs
// (without persisting) when the chat already holds MaxJobsPerChat jobs. A nil
// store is a no-op that returns "" so callers stay nil-safe when the store failed
// to open.
func (s *Store) Add(job Job) (string, error) {
	if s == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs[job.ChatID]) >= MaxJobsPerChat {
		return "", ErrTooManyJobs
	}
	id := strconv.FormatUint(s.nextID, 10)
	s.nextID++
	job.ID = id
	s.jobs[job.ChatID] = append(s.jobs[job.ChatID], job)
	if err := s.persistLocked(); err != nil {
		return "", err
	}
	return id, nil
}

// Remove hard-deletes the job with id from chatID's slice and rewrites the file;
// if the slice becomes empty the chat key is dropped. Removing an absent chat/id
// is a no-op (no needless rewrite). A nil store is a no-op.
func (s *Store) Remove(chatID, id string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, ok := s.jobs[chatID]
	if !ok {
		return nil
	}
	idx := indexOf(jobs, id)
	if idx < 0 {
		return nil
	}
	jobs = append(jobs[:idx], jobs[idx+1:]...)
	if len(jobs) == 0 {
		delete(s.jobs, chatID)
	} else {
		s.jobs[chatID] = jobs
	}
	return s.persistLocked()
}

// SetActive sets the Active flag of the job with id in chatID's slice and
// persists the change. A missing chat/id is a no-op. A nil store is a no-op.
func (s *Store) SetActive(chatID, id string, active bool) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := s.jobs[chatID]
	idx := indexOf(jobs, id)
	if idx < 0 {
		return nil
	}
	jobs[idx].Active = active
	return s.persistLocked()
}

// MarkFired records minuteKey as the last-fired minute of the job with id in
// chatID's slice and persists the change, so the scheduler's de-dup skips it for
// the rest of that minute. A missing chat/id is a no-op. A nil store is a no-op.
func (s *Store) MarkFired(chatID, id, minuteKey string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := s.jobs[chatID]
	idx := indexOf(jobs, id)
	if idx < 0 {
		return nil
	}
	jobs[idx].LastFired = minuteKey
	return s.persistLocked()
}

// List returns a copy of chatID's jobs in stable order (active and paused alike),
// so a caller can render the chat's schedule without racing a concurrent mutation.
// A nil store yields nil.
func (s *Store) List(chatID string) []Job {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := s.jobs[chatID]
	if len(jobs) == 0 {
		return nil
	}
	return append([]Job(nil), jobs...)
}

// ActiveJobs returns a copy of every chat's ACTIVE jobs (paused jobs excluded),
// which is what the scheduler reads each tick to decide what is due. A nil store
// yields nil.
func (s *Store) ActiveJobs() []Job {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Job
	for _, jobs := range s.jobs {
		for _, j := range jobs {
			if j.Active {
				out = append(out, j)
			}
		}
	}
	return out
}

// indexOf returns the index of the job with id in jobs, or -1.
func indexOf(jobs []Job, id string) int {
	for i := range jobs {
		if jobs[i].ID == id {
			return i
		}
	}
	return -1
}

// persistLocked writes the current map to a temp file in the same directory and
// renames it over the target, so a crash mid-write never leaves a half-written or
// corrupt store. The caller must hold s.mu.
func (s *Store) persistLocked() error {
	data, err := json.Marshal(s.jobs)
	if err != nil {
		return fmt.Errorf("schedule: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".schedule-*.tmp"); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	return nil
}
