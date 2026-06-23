package schedule

import (
	"context"
	"errors"
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
// fired job's prompt into its chat (chat.Service.Inject). fire MUST NOT block the
// caller: TickOnce runs on the single scheduler goroutine, so a blocking fire
// would stall every other chat's jobs — the production wiring runs Inject on its
// own goroutine (see buildScheduler). now supplies the current time (time.Now in
// production, a stub in tests); a nil log defaults to slog.Default().
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

// TickOnce performs one scheduling pass: fire every active job whose schedule
// matches the current wall-clock minute IN THAT CHAT'S TIMEZONE and that has not
// already fired this minute, then record the fire. Run calls it on every tick; it
// is exported so tests can drive scheduling deterministically with a stepped now
// func instead of waiting on real time. A spec that no longer parses is logged and
// skipped (never fired).
func (m *Manager) TickOnce() {
	if m.store == nil || m.fire == nil {
		return
	}
	now := m.now()
	locs := map[string]*time.Location{} // resolve each chat's zone once per tick
	for _, job := range m.store.ActiveJobs() {
		// Evaluate the job against the wall clock in its chat's timezone (a chat with
		// no zone set falls back to process-local time). The de-dup minuteKey is in
		// that same zone, so at-most-once-per-matching-minute holds per chat. A zone
		// changed mid-fire is best-effort: the stored key was written in the old zone,
		// so a job may fire once more under the new one — bounded and self-inflicted.
		loc, ok := locs[job.ChatID]
		if !ok {
			loc = m.location(job.ChatID)
			locs[job.ChatID] = loc
		}
		local := now
		if loc != nil {
			local = now.In(loc)
		}
		minuteKey := local.Format(minuteKeyLayout)
		if job.LastFired == minuteKey {
			continue
		}
		sched, err := Parse(job.Cron)
		if err != nil {
			m.log.Warn("scheduler: skipping job with invalid cron", "id", job.ID, "chat", job.ChatID, "cron", job.Cron, "error", err)
			continue
		}
		if !sched.Matches(local) {
			continue
		}
		m.fire(job.ChatID, job.Prompt)
		if err := m.store.MarkFired(job.ChatID, job.ID, minuteKey); err != nil {
			m.log.Warn("scheduler: mark fired failed", "id", job.ID, "chat", job.ChatID, "error", err)
		}
		m.log.Info("scheduler fired job", "id", job.ID, "chat", job.ChatID, "name", job.Name)
	}
}

// location resolves the IANA timezone a chat's jobs are evaluated in, or nil when
// the chat has no zone set (or the stored name fails to load — logged once per
// tick), in which case the caller uses the time as-is (the process-local default).
func (m *Manager) location(chatID string) *time.Location {
	name := m.store.Zone(chatID)
	if name == "" {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		m.log.Warn("scheduler: bad stored timezone; using process-local", "chat", chatID, "tz", name, "error", err)
		return nil
	}
	return loc
}

// usageText is the concise reply for an empty or unknown `/schedule` subcommand.
const usageText = "Usage:\n" +
	"/schedule list — list this chat's jobs\n" +
	"/schedule show <id> — show a job's full prompt\n" +
	"/schedule add <name> <min> <hour> <dom> <mon> <dow> <prompt…> — add a cron job\n" +
	"/schedule remove <id> — delete a job\n" +
	"/schedule pause <id> — pause a job\n" +
	"/schedule resume <id> — resume a paused job\n" +
	"/schedule tz [IANA] — show or set this chat's timezone (e.g. Europe/Moscow)\n\n" +
	"Cron fields: minute(0-59) hour(0-23) day-of-month(1-31) month(1-12) day-of-week(0-6, 0=Sun).\n" +
	"Each field supports *, n, a-b, a,b lists and */n steps.\n" +
	"Quote a name with spaces: /schedule add \"daily report\" 0 9 * * 1 post the update\n" +
	"Times use this chat's timezone (/schedule tz), else the server zone (UTC by default).\n" +
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
	case "show":
		return m.dispatchShow(chatID, rest)
	case "add":
		return m.dispatchAdd(chatID, userID, args)
	case "remove":
		return m.dispatchSetState(chatID, rest, stateRemove)
	case "pause":
		return m.dispatchSetState(chatID, rest, statePause)
	case "resume":
		return m.dispatchSetState(chatID, rest, stateResume)
	case "tz", "timezone":
		return m.dispatchTz(chatID, rest)
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
	header := "Scheduled jobs:"
	if z := m.store.Zone(chatID); z != "" {
		header = fmt.Sprintf("Scheduled jobs (timezone %s):", z)
	}
	lines := make([]string, 0, len(jobs)+1)
	lines = append(lines, header)
	for _, j := range jobs {
		preview := truncate(j.Prompt, promptPreviewRunes)
		lines = append(lines, fmt.Sprintf("#%s %s — `%s` (%s) — %s", j.ID, j.Name, j.Cron, stateLabel(j.Active), preview))
	}
	lines = append(lines, "Use /schedule show <id> for a job's full prompt.")
	return strings.Join(lines, "\n")
}

// dispatchShow renders one job's full detail (including the complete prompt),
// scoped to chatID. A foreign or unknown id is reported as "not found".
func (m *Manager) dispatchShow(chatID string, rest []string) string {
	if len(rest) < 1 {
		return "Usage: /schedule show <id>"
	}
	id := rest[0]
	for _, j := range m.store.List(chatID) {
		if j.ID != id {
			continue
		}
		zone := m.store.Zone(chatID)
		if zone == "" {
			zone = "server default"
		}
		return fmt.Sprintf(
			"Job #%s — %s\nCron: `%s` (%s, timezone %s)\nPrompt:\n%s",
			j.ID, j.Name, j.Cron, stateLabel(j.Active), zone, j.Prompt,
		)
	}
	return notFound(id)
}

// dispatchTz shows or sets this chat's IANA timezone. With no argument it reports
// the current zone; with one it validates the name (time.LoadLocation) and stores
// it; "none"/"clear" reverts to the server default. The zone governs how every job
// in this chat is matched against the wall clock.
func (m *Manager) dispatchTz(chatID string, rest []string) string {
	if len(rest) == 0 {
		if z := m.store.Zone(chatID); z != "" {
			return fmt.Sprintf("This chat's timezone is %s.", z)
		}
		return "This chat has no timezone set; schedules use the server zone (UTC by default).\n" +
			"Set one with /schedule tz <IANA>, e.g. /schedule tz Europe/Moscow."
	}
	name := rest[0]
	if strings.EqualFold(name, "none") || strings.EqualFold(name, "clear") {
		if err := m.store.SetZone(chatID, ""); err != nil {
			m.log.Error("schedule: clear timezone failed", "chat", chatID, "error", err)
			return "Failed to update the timezone. Please try again."
		}
		return "Cleared this chat's timezone; schedules now use the server zone."
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Sprintf("Unknown timezone %q. Use an IANA name like Europe/Moscow or UTC.", name)
	}
	if err := m.store.SetZone(chatID, name); err != nil {
		m.log.Error("schedule: set timezone failed", "chat", chatID, "error", err)
		return "Failed to update the timezone. Please try again."
	}
	return fmt.Sprintf("Timezone set to %s. Existing and new jobs in this chat now use it.", name)
}

// addUsage is the one-line grammar reminder for a malformed `add`.
const addUsage = "Usage: /schedule add <name> <min> <hour> <dom> <mon> <dow> <prompt…>\n" +
	"Quote a name that contains spaces, e.g. /schedule add \"daily report\" 0 9 * * 1 post it."

// dispatchAdd validates and persists a new job. The leading token is the name
// (optionally a double-quoted multi-word string), the next five tokens are the
// cron fields, and everything after them — internal spacing preserved — is the
// prompt. An invalid cron, an empty name, or an empty prompt is rejected WITHOUT
// persisting.
func (m *Manager) dispatchAdd(chatID string, userID int64, args string) string {
	name, cronSpec, prompt, err := parseAddArgs(afterFirstToken(args))
	if err != nil {
		return addUsage
	}
	if strings.TrimSpace(name) == "" {
		return "The job name cannot be empty."
	}
	if _, perr := Parse(cronSpec); perr != nil {
		return fmt.Sprintf("Invalid cron spec: %s", perr)
	}
	if strings.TrimSpace(prompt) == "" {
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
		if errors.Is(err, ErrTooManyJobs) {
			return fmt.Sprintf("This chat already has the maximum of %d scheduled jobs. Remove one before adding another.", MaxJobsPerChat)
		}
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
		return notFound(id)
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

// stateLabel renders a job's active/paused state for display.
func stateLabel(active bool) string {
	if active {
		return "active"
	}
	return "paused"
}

// notFound is the reply for a job id that is unknown in (or foreign to) the chat.
func notFound(id string) string {
	return fmt.Sprintf("Job #%s not found in this chat.", id)
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

// promptPreviewRunes bounds the prompt preview shown in `/schedule list`; the full
// prompt is available via `/schedule show <id>`.
const promptPreviewRunes = 50

// parseAddArgs parses the body of an `add` subcommand (everything after the "add"
// token) into the job name, the five-field cron spec, and the prompt. The name is
// either a single whitespace-delimited token or a double-quoted multi-word string
// ("daily report"); the next five tokens are the cron fields; the remainder — with
// its internal spacing preserved — is the prompt. It errors only when the name or
// the five cron tokens are missing (an empty prompt is left for the caller to
// reject with a clearer message).
func parseAddArgs(body string) (name, cronSpec, prompt string, err error) {
	name, rest, err := takeName(body)
	if err != nil {
		return "", "", "", err
	}
	fields := make([]string, 0, cronFields)
	for i := 0; i < cronFields; i++ {
		var tok string
		tok, rest = takeToken(rest)
		if tok == "" {
			return "", "", "", errors.New("missing cron fields")
		}
		fields = append(fields, tok)
	}
	cronSpec = strings.Join(fields, " ")
	prompt = strings.TrimLeftFunc(rest, unicode.IsSpace)
	return name, cronSpec, prompt, nil
}

// afterFirstToken returns s with its leading whitespace-delimited token (the
// subcommand verb) removed, preserving the remainder.
func afterFirstToken(s string) string {
	_, rest := takeToken(s)
	return rest
}

// takeToken splits off the first whitespace-delimited token of s (after trimming
// leading whitespace) and returns it plus the remainder, which begins at the
// whitespace before the next token (or "" when none remains).
func takeToken(s string) (token, rest string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// takeName splits off the job name — a double-quoted "multi word" string or a
// single whitespace-delimited token — and returns it plus the remainder. An empty
// or unterminated-quote name is an error.
func takeName(s string) (name, rest string, err error) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if s == "" {
		return "", "", errors.New("empty name")
	}
	if s[0] == '"' {
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return "", "", errors.New("unterminated quoted name")
		}
		name = s[1 : 1+end]
		if strings.TrimSpace(name) == "" {
			return "", "", errors.New("empty name")
		}
		return name, s[1+end+1:], nil
	}
	name, rest = takeToken(s)
	return name, rest, nil
}

// truncate collapses newlines and clips s to at most n runes (appending an
// ellipsis when clipped), for a one-line job preview.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
