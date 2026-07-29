//nolint:testpackage // whitebox: the sweep is unexported, and it is the unit under test.
package followup

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSweepSkipsTheStoreWhilePaused is the data-loss regression. Due is
// take-then-fire: it REMOVES what it returns. Sweeping while firing is impossible
// would therefore delete each due follow-up permanently — on an unauthorized
// deployment every matured followup/<delay>.md would vanish, and the sign-in
// would not bring it back. Pausing has to skip the sweep, not just the fire.
func TestSweepSkipsTheStoreWhilePaused(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "followups.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Add("1", "check the deploy", time.UnixMilli(1)); err != nil {
		t.Fatalf("add item: %v", err)
	}
	now := time.UnixMilli(1000)

	var fired []Item
	sweep(store, now, func() bool { return true }, func(it Item) { fired = append(fired, it) })

	if len(fired) != 0 {
		t.Errorf("fired %+v while paused, want nothing", fired)
	}
	if got := store.Count("1"); got != 1 {
		t.Fatalf("the store holds %d items while paused, want the item kept for later", got)
	}

	// Unpaused, the same item fires — it survived the pause.
	sweep(store, now, func() bool { return false }, func(it Item) { fired = append(fired, it) })

	if len(fired) != 1 || fired[0].Prompt != "check the deploy" {
		t.Errorf("fired %+v after the pause cleared, want the item that waited", fired)
	}
	if got := store.Count("1"); got != 0 {
		t.Errorf("the store still holds %d items after firing, want them taken", got)
	}
}

// TestSweepWithoutAPausePredicate: a nil predicate never pauses.
func TestSweepWithoutAPausePredicate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "followups.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Add("1", "go", time.UnixMilli(1)); err != nil {
		t.Fatalf("add item: %v", err)
	}

	var fired []Item
	sweep(store, time.UnixMilli(1000), nil, func(it Item) { fired = append(fired, it) })

	if len(fired) != 1 {
		t.Errorf("fired %+v with a nil predicate, want the due item", fired)
	}
}
