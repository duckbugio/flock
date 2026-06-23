package schedule_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/duckbugio/flock/core/schedule"
)

// newStore opens a fresh store in a temp dir.
func newStore(t *testing.T) (*schedule.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedules.json")
	s, err := schedule.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// addJob is a small helper to add an active job and return its id.
func addJob(t *testing.T, s *schedule.Store, chatID, name, cron, prompt string) string {
	t.Helper()
	id, err := s.Add(schedule.Job{ChatID: chatID, Name: name, Cron: cron, Prompt: prompt, Active: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return id
}

// TestStoreRoundTrip asserts a job persists across an Open→Add→reopen→List cycle
// with all fields intact.
func TestStoreRoundTrip(t *testing.T) {
	s, path := newStore(t)
	id := addJob(t, s, "100", "morning", "0 9 * * *", "stand up")

	// Reopen the same path: the persisted job must come back.
	s2, err := schedule.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	jobs := s2.List("100")
	if len(jobs) != 1 {
		t.Fatalf("after reopen List(100) = %d jobs, want 1", len(jobs))
	}
	got := jobs[0]
	if got.ID != id || got.Name != "morning" || got.Cron != "0 9 * * *" || got.Prompt != "stand up" || !got.Active {
		t.Errorf("round-tripped job = %+v, want id=%s morning/cron/prompt active", got, id)
	}
}

// TestStoreIDsMonotonicAcrossReopen asserts a new Add after a reopen never reuses
// a persisted id (the counter is seeded above the max persisted id on load).
func TestStoreIDsMonotonicAcrossReopen(t *testing.T) {
	s, path := newStore(t)
	id1 := addJob(t, s, "1", "a", "* * * * *", "p1")

	s2, err := schedule.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	id2 := addJob(t, s2, "1", "b", "* * * * *", "p2")
	if id1 == id2 {
		t.Fatalf("id reused after reopen: %s == %s", id1, id2)
	}
}

// TestStorePerChatIsolation asserts List only returns the queried chat's jobs.
func TestStorePerChatIsolation(t *testing.T) {
	s, _ := newStore(t)
	addJob(t, s, "100", "a", "* * * * *", "for 100")
	addJob(t, s, "200", "b", "* * * * *", "for 200")

	if got := s.List("100"); len(got) != 1 || got[0].ChatID != "100" {
		t.Errorf("List(100) = %+v, want one job for chat 100", got)
	}
	if got := s.List("200"); len(got) != 1 || got[0].ChatID != "200" {
		t.Errorf("List(200) = %+v, want one job for chat 200", got)
	}
	if got := s.List("999"); got != nil {
		t.Errorf("List(999) = %+v, want nil for an unknown chat", got)
	}
}

// TestStorePauseResume asserts SetActive toggles Active and ActiveJobs reflects it.
func TestStorePauseResume(t *testing.T) {
	s, _ := newStore(t)
	id := addJob(t, s, "100", "a", "* * * * *", "p")

	if got := len(s.ActiveJobs()); got != 1 {
		t.Fatalf("ActiveJobs = %d before pause, want 1", got)
	}
	if err := s.SetActive("100", id, false); err != nil {
		t.Fatalf("SetActive(false): %v", err)
	}
	if got := s.ActiveJobs(); len(got) != 0 {
		t.Errorf("ActiveJobs = %d after pause, want 0", len(got))
	}
	// List still shows the paused job.
	if got := s.List("100"); len(got) != 1 || got[0].Active {
		t.Errorf("List after pause = %+v, want one paused job", got)
	}
	if err := s.SetActive("100", id, true); err != nil {
		t.Fatalf("SetActive(true): %v", err)
	}
	if got := len(s.ActiveJobs()); got != 1 {
		t.Errorf("ActiveJobs = %d after resume, want 1", got)
	}
}

// TestStoreRemove asserts Remove hard-deletes the job and drops an emptied chat
// key, and that removing an absent id is a harmless no-op.
func TestStoreRemove(t *testing.T) {
	s, _ := newStore(t)
	id := addJob(t, s, "100", "a", "* * * * *", "p")

	// Removing an absent id is a no-op.
	if err := s.Remove("100", "999"); err != nil {
		t.Fatalf("Remove(absent): %v", err)
	}
	if got := len(s.List("100")); got != 1 {
		t.Fatalf("List after no-op remove = %d, want 1", got)
	}

	if err := s.Remove("100", id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := s.List("100"); got != nil {
		t.Errorf("List after remove = %+v, want nil (chat key dropped)", got)
	}
}

// TestStoreMarkFiredDeDup asserts MarkFired records the minute key so a re-check
// for the same minute is suppressed (the scheduler reads LastFired to de-dup).
func TestStoreMarkFiredDeDup(t *testing.T) {
	s, _ := newStore(t)
	id := addJob(t, s, "100", "a", "* * * * *", "p")

	const key = "2026-06-18T09:00"
	if err := s.MarkFired("100", id, key); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}
	jobs := s.ActiveJobs()
	if len(jobs) != 1 || jobs[0].LastFired != key {
		t.Fatalf("ActiveJobs after MarkFired = %+v, want LastFired=%q", jobs, key)
	}
}

// TestStoreAddCapPerChat asserts Add enforces MaxJobsPerChat: filling a chat to
// the cap succeeds, the next Add is rejected with ErrTooManyJobs and not
// persisted, and a DIFFERENT chat is unaffected (the cap is per-chat).
func TestStoreAddCapPerChat(t *testing.T) {
	s, _ := newStore(t)
	for i := 0; i < schedule.MaxJobsPerChat; i++ {
		if _, err := s.Add(schedule.Job{ChatID: "100", Name: "j", Cron: "* * * * *", Prompt: "p", Active: true}); err != nil {
			t.Fatalf("Add #%d within cap: %v", i, err)
		}
	}
	_, err := s.Add(schedule.Job{ChatID: "100", Name: "over", Cron: "* * * * *", Prompt: "p", Active: true})
	if !errors.Is(err, schedule.ErrTooManyJobs) {
		t.Fatalf("Add past cap err = %v, want ErrTooManyJobs", err)
	}
	if got := len(s.List("100")); got != schedule.MaxJobsPerChat {
		t.Errorf("chat 100 has %d jobs, want the cap %d (rejected add not persisted)", got, schedule.MaxJobsPerChat)
	}
	// The cap is per-chat: a different chat can still add.
	if _, err := s.Add(schedule.Job{ChatID: "200", Name: "ok", Cron: "* * * * *", Prompt: "p", Active: true}); err != nil {
		t.Errorf("Add to a different chat err = %v, want nil (cap is per-chat)", err)
	}
}

// TestStoreZoneRoundTrip asserts a chat's timezone is persisted across a reopen,
// is per-chat, and that an empty SetZone clears it.
func TestStoreZoneRoundTrip(t *testing.T) {
	s, path := newStore(t)
	if err := s.SetZone("100", tzMoscow); err != nil {
		t.Fatalf("SetZone: %v", err)
	}
	if got := s.Zone("100"); got != tzMoscow {
		t.Errorf("Zone(100) = %q, want Europe/Moscow", got)
	}
	if got := s.Zone("200"); got != "" {
		t.Errorf("Zone(200) = %q, want empty (per-chat)", got)
	}

	// Persisted across reopen.
	s2, err := schedule.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Zone("100"); got != tzMoscow {
		t.Errorf("after reopen Zone(100) = %q, want Europe/Moscow", got)
	}

	// Clearing reverts to empty.
	if err := s2.SetZone("100", ""); err != nil {
		t.Fatalf("clear SetZone: %v", err)
	}
	if got := s2.Zone("100"); got != "" {
		t.Errorf("after clear Zone(100) = %q, want empty", got)
	}
}

// TestStoreZoneSurvivesAlongsideJobs asserts jobs and zones round-trip together
// (the persisted format carries both).
func TestStoreZoneSurvivesAlongsideJobs(t *testing.T) {
	s, path := newStore(t)
	addJob(t, s, "100", "a", "0 9 * * 1", "p")
	if err := s.SetZone("100", "UTC"); err != nil {
		t.Fatalf("SetZone: %v", err)
	}
	s2, err := schedule.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(s2.List("100")); got != 1 {
		t.Errorf("after reopen jobs = %d, want 1", got)
	}
	if got := s2.Zone("100"); got != "UTC" {
		t.Errorf("after reopen zone = %q, want UTC", got)
	}
}

// TestStoreNilSafe asserts a nil store's mutators are no-ops and its readers
// return empty, matching the core/pending nil-safety contract.
func TestStoreNilSafe(t *testing.T) {
	var s *schedule.Store
	if id, err := s.Add(schedule.Job{ChatID: "1"}); id != "" || err != nil {
		t.Errorf("nil Add = (%q,%v), want ('',nil)", id, err)
	}
	if err := s.Remove("1", "1"); err != nil {
		t.Errorf("nil Remove err = %v", err)
	}
	if err := s.SetActive("1", "1", true); err != nil {
		t.Errorf("nil SetActive err = %v", err)
	}
	if err := s.MarkFired("1", "1", "k"); err != nil {
		t.Errorf("nil MarkFired err = %v", err)
	}
	if err := s.SetZone("1", "UTC"); err != nil {
		t.Errorf("nil SetZone err = %v", err)
	}
	if got := s.Zone("1"); got != "" {
		t.Errorf("nil Zone = %q, want empty", got)
	}
	if got := s.List("1"); got != nil {
		t.Errorf("nil List = %+v, want nil", got)
	}
	if got := s.ActiveJobs(); got != nil {
		t.Errorf("nil ActiveJobs = %+v, want nil", got)
	}
}
