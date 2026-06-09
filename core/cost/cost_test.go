//nolint:testpackage // intentionally whitebox to test unexported cost ledger internals
package cost

import (
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "costs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s, path
}

// TestAddAccumulatesAndCaps covers the accumulate-then-deny boundary: a user is
// allowed until their accumulated total reaches the cap, after which Allowed is
// false.
func TestAddAccumulatesAndCaps(t *testing.T) {
	s, _ := openTemp(t)
	const (
		user     int64   = 100
		capLimit float64 = 1.0
	)

	if !s.Allowed(user, capLimit) {
		t.Fatal("fresh user should be under the cap")
	}
	if err := s.Add(user, 0.4); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !s.Allowed(user, capLimit) {
		t.Fatal("0.4 < 1.0 cap, should still be allowed")
	}
	if err := s.Add(user, 0.6); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Total is now exactly 1.0 == cap: Allowed denies at >= cap (reactive cap).
	if s.Allowed(user, capLimit) {
		t.Fatal("total == cap should deny the next request")
	}
}

// TestAddZeroAndNegativeNoop confirms a non-positive cost does not move the total
// (a failed/stopped run records nothing).
func TestAddZeroAndNegativeNoop(t *testing.T) {
	s, _ := openTemp(t)
	const user int64 = 1
	if err := s.Add(user, 0); err != nil {
		t.Fatalf("add 0: %v", err)
	}
	if err := s.Add(user, -5); err != nil {
		t.Fatalf("add negative: %v", err)
	}
	if !s.Allowed(user, 0.0001) {
		t.Fatal("no cost recorded, should be under any positive cap")
	}
}

// TestDisabledCap confirms a non-positive cap always allows regardless of spend.
func TestDisabledCap(t *testing.T) {
	s, _ := openTemp(t)
	const user int64 = 1
	if err := s.Add(user, 999); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !s.Allowed(user, 0) {
		t.Fatal("cap <= 0 must disable the gate (always allowed)")
	}
	if !s.Allowed(user, -1) {
		t.Fatal("negative cap must disable the gate (always allowed)")
	}
}

// TestNilStoreNoop confirms a nil *Store is a safe always-allowed no-op.
func TestNilStoreNoop(t *testing.T) {
	var s *Store
	if !s.Allowed(1, 1.0) {
		t.Fatal("nil store must always allow")
	}
	if err := s.Add(1, 5); err != nil {
		t.Fatalf("nil store Add should be a no-op, got %v", err)
	}
}

// TestPersistenceAcrossReopen confirms accumulated totals survive a fresh Open of
// the same path (a process restart).
func TestPersistenceAcrossReopen(t *testing.T) {
	s, path := openTemp(t)
	const user int64 = 77
	if err := s.Add(user, 0.5); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.Add(user, 0.75); err != nil {
		t.Fatalf("add: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// Total is 1.25; a 1.0 cap must now deny on the reopened store (totals restored).
	if reopened.Allowed(user, 1.0) {
		t.Fatal("reopened store should have restored the 1.25 total and deny a 1.0 cap")
	}
	// And a higher cap still allows, proving the exact total was restored, not just
	// "some non-zero".
	if !reopened.Allowed(user, 2.0) {
		t.Fatal("reopened store should allow under a 2.0 cap (total is 1.25)")
	}
}

// TestAddConcurrent exercises concurrent Adds for one user under the race
// detector and asserts the accumulated total is exact.
func TestAddConcurrent(t *testing.T) {
	s, _ := openTemp(t)
	const user int64 = 9
	const (
		workers = 200
		each    = 0.01
	)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := s.Add(user, each); err != nil {
				t.Errorf("add: %v", err)
			}
		}()
	}
	wg.Wait()

	// Total is workers*each = 2.0. A cap just below must deny; just above must allow.
	if s.Allowed(user, 1.99) {
		t.Fatal("after concurrent adds total should be ~2.0, denying a 1.99 cap")
	}
	if !s.Allowed(user, 2.01) {
		t.Fatal("after concurrent adds total should be ~2.0, allowing a 2.01 cap")
	}
}
