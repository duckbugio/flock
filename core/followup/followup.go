// Package followup gives a run a LEGAL way to come back later. The failure
// mode it exists for: the agent ends a turn with "I'll report back when X
// finishes" — but nothing ever wakes it (each turn is a one-shot process), so
// the promise silently strands the user. Instead of promising, a run writes a
// file into the workspace's followup/ directory (see the convention appended
// to every workspace CLAUDE.md): the filename is the delay ("15m.md", "2h.md"),
// the content is the prompt to run then. The service sweeps the directory
// after every run into this durable store, and a background loop fires each
// item once at its due time through the same autonomy-budgeted injection path
// the other loops use.
//
// The store mirrors the other JSON file stores (single file, atomic rewrite,
// mutex, monotonic ids seeded above the max persisted id).
package followup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/duckbugio/flock/internal/atomicfile"
)

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// MinDelay floors a follow-up delay: anything shorter belongs in the
// foreground of the turn that wanted it.
const MinDelay = time.Minute

// MaxDelay caps a follow-up delay; a longer horizon belongs in /schedule.
const MaxDelay = 48 * time.Hour

// MaxPerChat bounds pending follow-ups per chat — each is an unattended,
// model-cost-bearing run, so an unbounded pile is an autonomy amplifier.
const MaxPerChat = 10

// ErrTooMany is returned by Add when a chat is at MaxPerChat, so the sweeper
// can surface a clear notice instead of a generic failure.
var ErrTooMany = errors.New("followup: chat has reached the maximum number of pending follow-ups")

// Item is one scheduled one-shot follow-up.
type Item struct {
	ID     string `json:"id"`
	ChatID string `json:"chatId"`
	Prompt string `json:"prompt"`
	DueAt  int64  `json:"dueAt"` // Unix ms
}

// FileStore is a JSON-file-backed store of pending follow-ups, safe for
// concurrent use.
type FileStore struct {
	path string

	mu     sync.Mutex
	items  map[string][]Item // chatID -> pending items
	nextID uint64
}

// Open loads (or creates) a FileStore at path. A missing file yields an empty
// store; the parent directory is created if needed.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("followup: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("followup: create store dir: %w", err)
	}
	s := &FileStore{path: path, items: map[string][]Item{}, nextID: 1}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file, seeding the id counter above the
// max persisted id so a new Add after a restart never collides.
func (s *FileStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("followup: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &s.items); err != nil {
		return fmt.Errorf("followup: parse store %s: %w", s.path, err)
	}
	for _, q := range s.items {
		for _, it := range q {
			if n, err := strconv.ParseUint(it.ID, 10, 64); err == nil && n >= s.nextID {
				s.nextID = n + 1
			}
		}
	}
	return nil
}

// Add schedules prompt to fire at due, enforcing the per-chat cap.
func (s *FileStore) Add(chatID, prompt string, due time.Time) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items[chatID]) >= MaxPerChat {
		return Item{}, ErrTooMany
	}
	it := Item{
		ID:     strconv.FormatUint(s.nextID, 10),
		ChatID: chatID,
		Prompt: prompt,
		DueAt:  due.UnixMilli(),
	}
	s.nextID++
	s.items[chatID] = append(s.items[chatID], it)
	if err := s.persistLocked(); err != nil {
		return Item{}, err
	}
	return it, nil
}

// Due returns every item due at or before now, REMOVING them from the store
// (take-then-fire: a crash between take and fire drops at most one firing —
// for unattended work a miss is cheaper than a duplicate).
func (s *FileStore) Due(now time.Time) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowMs := now.UnixMilli()
	var due []Item
	changed := false
	for chatID, q := range s.items {
		var keep []Item
		for _, it := range q {
			if it.DueAt <= nowMs {
				due = append(due, it)
				changed = true
				continue
			}
			keep = append(keep, it)
		}
		if len(keep) == 0 {
			delete(s.items, chatID)
		} else {
			s.items[chatID] = keep
		}
	}
	if changed {
		if err := s.persistLocked(); err != nil {
			// The items are already detached in memory; a persist failure only risks
			// a duplicate fire after a crash, which the caller tolerates.
			return due
		}
	}
	return due
}

// Count returns how many follow-ups chatID has pending.
func (s *FileStore) Count(chatID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items[chatID])
}

// persistLocked writes the current map atomically. The caller must hold s.mu.
func (s *FileStore) persistLocked() error {
	data, err := json.Marshal(s.items)
	if err != nil {
		return fmt.Errorf("followup: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".followup-*.tmp"); err != nil {
		return fmt.Errorf("followup: %w", err)
	}
	return nil
}

// tick is the firing loop's poll period — coarse on purpose (delays are
// minutes to hours) and identical to the cron scheduler's cadence.
const tick = 30 * time.Second

// Run fires due follow-ups until ctx is cancelled, handing each to fire (in
// production a closure over chat.Service.InjectAuto). It returns ctx.Err() on
// cancellation.
//
// paused reports that firing is impossible right now (in production: the AI
// provider is unauthorized). The WHOLE sweep is skipped while it holds — Due is
// not called at all. That is the point: Due is take-then-fire, removing what it
// returns, so sweeping into a fire that drops the item would delete scheduled
// work permanently. Skipping instead leaves each item in the store, due, to fire
// once the block clears. A nil paused never pauses.
func Run(ctx context.Context, store *FileStore, now func() time.Time, paused func() bool, fire func(Item)) error {
	if now == nil {
		now = time.Now
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			sweep(store, now(), paused, fire)
		}
	}
}

// sweep fires everything due, unless firing is impossible right now.
//
// The pause check comes BEFORE Due deliberately: Due is take-then-fire, removing
// what it returns, so sweeping into a fire that would drop the item deletes
// scheduled work permanently.
func sweep(store *FileStore, now time.Time, paused func() bool, fire func(Item)) {
	if paused != nil && paused() {
		return
	}
	for _, it := range store.Due(now) {
		fire(it)
	}
}

// ParseDelay parses a follow-up filename stem ("15m", "2h", "90s", "1d") into
// a delay clamped to [MinDelay, MaxDelay]. ok=false for anything that does not
// parse as a positive Go-style duration (with "d" accepted as 24h).
func ParseDelay(stem string) (time.Duration, bool) {
	if stem == "" {
		return 0, false
	}
	// Accept a trailing 'd' (days) which time.ParseDuration lacks.
	if last := stem[len(stem)-1]; last == 'd' {
		n, err := strconv.Atoi(stem[:len(stem)-1])
		if err != nil || n <= 0 {
			return 0, false
		}
		return clampDelay(time.Duration(n) * 24 * time.Hour), true
	}
	d, err := time.ParseDuration(stem)
	if err != nil || d <= 0 {
		return 0, false
	}
	return clampDelay(d), true
}

// clampDelay bounds a delay to [MinDelay, MaxDelay].
func clampDelay(d time.Duration) time.Duration {
	if d < MinDelay {
		return MinDelay
	}
	if d > MaxDelay {
		return MaxDelay
	}
	return d
}
