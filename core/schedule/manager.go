package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// minuteKeyLayout is the timestamp layout for a job's de-dup minute key. Two
// times in the same minute (local zone) produce the same key, so a job fires at
// most once per matching minute even with a sub-minute scheduler tick.
const minuteKeyLayout = "2006-01-02T15:04"

// tickInterval is the scheduler's polling period. It is sub-minute so the loop
// never skips a matching minute on small clock drift; the per-job minute-key
// de-dup guarantees at most one fire per matching minute regardless.
const tickInterval = 30 * time.Second

// addFields is the number of fixed leading tokens an `add` subcommand carries
// before the prompt: name + the 5 cron fields.
const addFields = 6

// Manager runs the background scheduler and serves the transport-neutral
// `/schedule` subcommands. It owns no transport state: a chat id is a plain
// string and the fire callback (wired to chat.Service.Inject) is what couples a
// due job to the dispatch lane.
type Manager struct {
	store *Store
	fire  func(chatID, prompt string)
	now   func() time.Time
	log   *slog.Logger
}

// NewManager builds a Manager. store is the durable job store; fire injects a
// fired job's prompt into its chat (chat.Service.Inject); now supplies the current
// time (time.Now in production, a stub in tests); a nil log defaults to
// slog.Default().
func NewManager(store *Store, fire func(chatID, prompt string), now func() time.Time, log *slog.Logger) *Manager {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{store: store, fire: fire, now: now, log: log}
}

// Run drives the scheduler until ctx is cancelled: every tick it reads the LIVE
// store (so a job added between ticks fires on its next match — hot reload with no
// restart) and fires every due active job exactly once per matching minute. It
// returns ctx.Err() on cancellation.
func (m *Manager) Run(ctx context.Context) error {
	m.log.Info("scheduler started", "tick", tickInterval)
	for {
		m.TickOnce()
		select {
		case <-time.After(tickInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TickOnce performs one scheduling pass: for the current minute key, fire every
// active job whose schedule matches now() and that has not already fired this
// minute, then record the fire. Run calls it on every tick; it is exported so
// tests can drive scheduling deterministically with a stepped now func instead of
// waiting on real time. A spec that no longer parses is logged and skipped (never
// fired).
func (m *Manager) TickOnce() {
	if m.store == nil || m.fire == nil {
		return
	}
	now := m.now()
	minuteKey := now.Format(minuteKeyLayout)
	for _, job := range m.store.ActiveJobs() {
		if job.LastFired == minuteKey {
			continue
		}
		sched, err := Parse(job.Cron)
		if err != nil {
			m.log.Warn("scheduler: skipping job with invalid cron", "id", job.ID, "chat", job.ChatID, "cron", job.Cron, "error", err)
			continue
		}
		if !sched.Matches(now) {
			continue
		}
		m.fire(job.ChatID, job.Prompt)
		if err := m.store.MarkFired(job.ChatID, job.ID, minuteKey); err != nil {
			m.log.Warn("scheduler: mark fired failed", "id", job.ID, "chat", job.ChatID, "error", err)
		}
		m.log.Info("scheduler fired job", "id", job.ID, "chat", job.ChatID, "name", job.Name)
	}
}

// usageText is the concise reply for an empty or unknown `/schedule` subcommand.
const usageText = "Usage:\n" +
	"/schedule list — list this chat's jobs\n" +
	"/schedule add <name> <min> <hour> <dom> <mon> <dow> <prompt…> — add a cron job\n" +
	"/schedule remove <id> — delete a job\n" +
	"/schedule pause <id> — pause a job\n" +
	"/schedule resume <id> — resume a paused job\n\n" +
	"Cron fields: minute(0-59) hour(0-23) day-of-month(1-31) month(1-12) day-of-week(0-6, 0=Sun).\n" +
	"Each field supports *, n, a-b, a,b lists and */n steps. Times are in the server's local zone.\n" +
	"Note: when both day-of-month and day-of-week are restricted a job fires only when BOTH match."

// Dispatch is the transport-neutral `/schedule` subcommand handler shared by both
// adapters: it parses args, mutates the store, and returns the reply text to send.
// chatID scopes every operation (a job is only ever read or mutated for its own
// chat — an unknown or foreign id is reported as "not found"). userID is recorded
// as the creator of an added job. An empty or unknown subcommand returns the usage
// string.
func (m *Manager) Dispatch(chatID string, userID int64, args string) string {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return usageText
	}
	sub := strings.ToLower(fields[0])
	rest := fields[1:]
	switch sub {
	case "list":
		return m.dispatchList(chatID)
	case "add":
		return m.dispatchAdd(chatID, userID, args, rest)
	case "remove":
		return m.dispatchSetState(chatID, rest, stateRemove)
	case "pause":
		return m.dispatchSetState(chatID, rest, statePause)
	case "resume":
		return m.dispatchSetState(chatID, rest, stateResume)
	default:
		return usageText
	}
}

// dispatchList renders this chat's jobs (active and paused), sorted by numeric id
// for a stable display.
func (m *Manager) dispatchList(chatID string) string {
	jobs := m.store.List(chatID)
	if len(jobs) == 0 {
		return "No scheduled jobs in this chat."
	}
	sort.Slice(jobs, func(i, j int) bool { return idLess(jobs[i].ID, jobs[j].ID) })
	lines := make([]string, 0, len(jobs)+1)
	lines = append(lines, "Scheduled jobs:")
	for _, j := range jobs {
		state := "active"
		if !j.Active {
			state = "paused"
		}
		lines = append(lines, fmt.Sprintf("#%s %s — `%s` (%s)", j.ID, j.Name, j.Cron, state))
	}
	return strings.Join(lines, "\n")
}

// dispatchAdd validates and persists a new job. The fixed leading tokens are the
// name and the five cron fields; everything after them (preserving the original
// spacing) is the prompt. An invalid cron, an empty name, or an empty prompt is
// rejected WITHOUT persisting.
func (m *Manager) dispatchAdd(chatID string, userID int64, args string, rest []string) string {
	if len(rest) < addFields+1 {
		return "Usage: /schedule add <name> <min> <hour> <dom> <mon> <dow> <prompt…>"
	}
	name := rest[0]
	if strings.TrimSpace(name) == "" {
		return "The job name cannot be empty."
	}
	cronSpec := strings.Join(rest[1:addFields], " ")
	if _, err := Parse(cronSpec); err != nil {
		return fmt.Sprintf("Invalid cron spec: %s", err)
	}
	// The prompt is everything after the "add" token, the name, and the 5 cron
	// fields (1 + addFields tokens); keep its original internal spacing verbatim.
	prompt := strings.TrimSpace(dropTokens(args, 1+addFields))
	if prompt == "" {
		return "The prompt cannot be empty."
	}
	id, err := m.store.Add(Job{
		ChatID:    chatID,
		Name:      name,
		Cron:      cronSpec,
		Prompt:    prompt,
		CreatedBy: userID,
		Active:    true,
		CreatedAt: m.now().Unix(),
	})
	if err != nil {
		m.log.Error("schedule: add job failed", "chat", chatID, "error", err)
		return "Failed to save the job. Please try again."
	}
	return fmt.Sprintf("Added job #%s.", id)
}

// jobState is the target of a state-changing subcommand (remove/pause/resume).
type jobState int

const (
	stateRemove jobState = iota
	statePause
	stateResume
)

// dispatchSetState applies a remove/pause/resume to a single job, scoped to
// chatID. The job must belong to this chat: a foreign or unknown id is reported as
// "not found" (never mutating another chat's job).
func (m *Manager) dispatchSetState(chatID string, rest []string, state jobState) string {
	if len(rest) < 1 {
		return "Usage: /schedule " + stateVerb(state) + " <id>"
	}
	id := rest[0]
	if !m.chatOwnsJob(chatID, id) {
		return fmt.Sprintf("Job #%s not found in this chat.", id)
	}
	var err error
	var ok string
	switch state {
	case stateRemove:
		err = m.store.Remove(chatID, id)
		ok = fmt.Sprintf("Removed job #%s.", id)
	case statePause:
		err = m.store.SetActive(chatID, id, false)
		ok = fmt.Sprintf("Paused job #%s.", id)
	case stateResume:
		err = m.store.SetActive(chatID, id, true)
		ok = fmt.Sprintf("Resumed job #%s.", id)
	}
	if err != nil {
		m.log.Error("schedule: update job failed", "chat", chatID, "id", id, "error", err)
		return "Failed to update the job. Please try again."
	}
	return ok
}

// chatOwnsJob reports whether chatID has a job with the given id, so a mutation
// can be rejected before it touches the store when the id is unknown or foreign.
func (m *Manager) chatOwnsJob(chatID, id string) bool {
	for _, j := range m.store.List(chatID) {
		if j.ID == id {
			return true
		}
	}
	return false
}

// stateVerb labels a jobState for a usage string.
func stateVerb(state jobState) string {
	switch state {
	case statePause:
		return "pause"
	case stateResume:
		return "resume"
	default:
		return "remove"
	}
}

// dropTokens returns s with its first n whitespace-delimited tokens (and the
// whitespace separating them) removed, preserving the remainder verbatim
// (including any internal spacing of the surviving text). It treats the same
// Unicode whitespace as strings.Fields so the token boundaries match how rest was
// split, while keeping the prompt's own internal spacing intact.
func dropTokens(s string, n int) string {
	for i := 0; i < n; i++ {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		j := strings.IndexFunc(s, unicode.IsSpace)
		if j < 0 {
			return ""
		}
		s = s[j:]
	}
	return strings.TrimLeftFunc(s, unicode.IsSpace)
}

// idLess orders two decimal job ids numerically (falling back to a string compare
// for any non-numeric id, which Add never produces).
func idLess(a, b string) bool {
	ai, aerr := strconv.ParseUint(a, 10, 64)
	bi, berr := strconv.ParseUint(b, 10, 64)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a < b
}
