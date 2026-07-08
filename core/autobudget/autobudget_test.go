//nolint:testpackage // intentionally whitebox to test unexported autobudget internals
package autobudget

import (
	"path/filepath"
	"testing"
	"time"
)

var day1 = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func TestNilStoreIsNoop(t *testing.T) {
	var s *Store
	if !s.Allowed("1", day1, 5) {
		t.Fatal("nil store must always allow")
	}
	if err := s.Add("1", day1, 1); err != nil {
		t.Fatalf("nil Add: %v", err)
	}
	if got := s.SpentToday("1", day1); got != 0 {
		t.Fatalf("nil SpentToday = %v, want 0", got)
	}
}

func TestAccumulateAndCap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auto.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Allowed("1", day1, 2) {
		t.Fatal("fresh store must allow")
	}
	if err := s.Add("1", day1, 1.5); err != nil {
		t.Fatal(err)
	}
	if !s.Allowed("1", day1, 2) {
		t.Fatal("1.5 < 2 must still allow")
	}
	if err := s.Add("1", day1, 0.6); err != nil {
		t.Fatal(err)
	}
	if s.Allowed("1", day1, 2) {
		t.Fatal("2.1 >= 2 must deny")
	}
	// Other chats and other days are independent buckets.
	if !s.Allowed("2", day1, 2) {
		t.Fatal("another chat must be unaffected")
	}
	if !s.Allowed("1", day1.AddDate(0, 0, 1), 2) {
		t.Fatal("the next day must reset the bucket")
	}
	// A non-positive cap disables the gate.
	if !s.Allowed("1", day1, 0) {
		t.Fatal("zero cap must always allow")
	}
}

func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("1", day1, 3); err != nil {
		t.Fatal(err)
	}
	re, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := re.SpentToday("1", day1); got != 3 {
		t.Fatalf("reopened SpentToday = %v, want 3", got)
	}
}

func TestPruneOldDays(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auto.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("1", day1, 1); err != nil {
		t.Fatal(err)
	}
	// A write 3 days later prunes the old bucket (retention is 2 days).
	later := day1.AddDate(0, 0, 3)
	if err := s.Add("1", later, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.SpentToday("1", day1); got != 0 {
		t.Fatalf("old day should be pruned, got %v", got)
	}
	if got := s.SpentToday("1", later); got != 1 {
		t.Fatalf("current day = %v, want 1", got)
	}
}

func TestNonPositiveAddIsNoop(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auto.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("1", day1, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("1", day1, -5); err != nil {
		t.Fatal(err)
	}
	if got := s.SpentToday("1", day1); got != 0 {
		t.Fatalf("SpentToday = %v, want 0", got)
	}
}
