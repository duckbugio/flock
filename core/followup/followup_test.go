package followup_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/followup"
)

var base = time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) *followup.FileStore {
	t.Helper()
	s, err := followup.Open(filepath.Join(t.TempDir(), "followups.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAddDueRemoves(t *testing.T) {
	s := openTemp(t)
	it, err := s.Add("100", "check the deploy", base.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if it.ID == "" || s.Count("100") != 1 {
		t.Fatalf("Add = %+v, count %d", it, s.Count("100"))
	}
	if due := s.Due(base.Add(10 * time.Minute)); len(due) != 0 {
		t.Fatalf("not yet due, got %+v", due)
	}
	due := s.Due(base.Add(16 * time.Minute))
	if len(due) != 1 || due[0].Prompt != "check the deploy" {
		t.Fatalf("Due = %+v", due)
	}
	// Take-then-fire: taken items are gone.
	if s.Count("100") != 0 {
		t.Fatalf("taken item must be removed, count %d", s.Count("100"))
	}
	if again := s.Due(base.Add(time.Hour)); len(again) != 0 {
		t.Fatalf("no double fire, got %+v", again)
	}
}

func TestPerChatCap(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < followup.MaxPerChat; i++ {
		if _, err := s.Add("100", "p", base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add("100", "one too many", base.Add(time.Hour)); err == nil {
		t.Fatal("cap must reject")
	}
	// Other chats are unaffected.
	if _, err := s.Add("200", "p", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "followups.json")
	s, err := followup.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Add("100", "p", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	re, err := followup.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if re.Count("100") != 1 {
		t.Fatalf("reopened count = %d", re.Count("100"))
	}
	second, err := re.Add("100", "q", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("id collision after reopen: %s", second.ID)
	}
}

func TestParseDelay(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"15m", 15 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"90s", 90 * time.Second, true},
		{"1d", 24 * time.Hour, true},
		{"30s", time.Minute, true},        // floored to MinDelay
		{"100h", followup.MaxDelay, true}, // capped
		{"3d", followup.MaxDelay, true},   // capped via the d-branch
		{"0m", 0, false},
		{"-5m", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := followup.ParseDelay(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("ParseDelay(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
