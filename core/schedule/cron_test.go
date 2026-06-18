package schedule_test

import (
	"testing"
	"time"

	"github.com/duckbugio/flock/core/schedule"
)

// atYear is the fixed year the matcher tests use; the weekday assertions below
// are pinned to dates in this year.
const atYear = 2026

// at builds a local time in atYear for the matcher tests. The matcher reads the
// time's own fields (minute/hour/day/month/weekday), so the location only needs to
// be consistent; UTC keeps the weekday deterministic across hosts.
func at(month time.Month, day, hour, minute int) time.Time {
	return time.Date(atYear, month, day, hour, minute, 0, 0, time.UTC)
}

// TestParseValid covers the accepted field grammar: "*", a single value, an
// inclusive range, a comma list, a "*/n" step, an "a-b/n" stepped range, and a
// combination of those in one field.
func TestParseValid(t *testing.T) {
	specs := []string{
		"* * * * *",
		"0 0 1 1 0",
		"59 23 31 12 6",
		"0-30 * * * *",
		"0,15,30,45 * * * *",
		"*/10 * * * *",
		"0-20/5 * * * *",
		"1,2,4-6,*/10 * * * *",
		"5 9 * * 1-5",
		"5/15 * * * *",  // n/step: minute 5 through max, step 15
		"0/15 * * * *",  // n/step from 0
		"59/30 * * * *", // n/step where n is the field max
	}
	for _, s := range specs {
		if _, err := schedule.Parse(s); err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", s, err)
		}
	}
}

// TestParseInvalid asserts malformed or out-of-range specs are rejected at parse
// time (so `add` can refuse without persisting).
func TestParseInvalid(t *testing.T) {
	specs := []string{
		"",              // empty
		"* * * *",       // too few fields
		"* * * * * *",   // too many fields
		"60 * * * *",    // minute out of range
		"* 24 * * *",    // hour out of range
		"* * 0 * *",     // day-of-month below 1
		"* * 32 * *",    // day-of-month above 31
		"* * * 0 *",     // month below 1
		"* * * 13 *",    // month above 12
		"* * * * 7",     // day-of-week above 6
		"5-1 * * * *",   // descending range
		"*/0 * * * *",   // zero step
		"*/-1 * * * *",  // negative step
		"a * * * *",     // non-numeric
		"1,,2 * * * *",  // empty list element
		"1- * * * *",    // incomplete range
		"1-2-3 * * * *", // malformed range
		"*/x * * * *",   // non-numeric step
		"5/0 * * * *",   // n/step with zero step
		"5/-1 * * * *",  // n/step with negative step
		"60/5 * * * *",  // n/step with out-of-range base
	}
	for _, s := range specs {
		if _, err := schedule.Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil error, want a rejection", s)
		}
	}
}

// TestMatches covers field semantics and boundary values: wildcards, single
// values, ranges, lists, and steps each match exactly the expected minutes/hours.
func TestMatches(t *testing.T) {
	tests := []struct {
		name string
		spec string
		t    time.Time
		want bool
	}{
		{"every minute matches", "* * * * *", at(6, 18, 12, 34), true},
		{"exact minute hit", "30 * * * *", at(6, 18, 12, 30), true},
		{"exact minute miss", "30 * * * *", at(6, 18, 12, 31), false},
		{"minute boundary 0", "0 * * * *", at(6, 18, 12, 0), true},
		{"minute boundary 59", "59 * * * *", at(6, 18, 12, 59), true},
		{"hour match", "* 9 * * *", at(6, 18, 9, 0), true},
		{"hour miss", "* 9 * * *", at(6, 18, 10, 0), false},
		{"hour boundary 23", "* 23 * * *", at(6, 18, 23, 0), true},
		{"range hit lower", "0-30 * * * *", at(6, 18, 12, 0), true},
		{"range hit upper", "0-30 * * * *", at(6, 18, 12, 30), true},
		{"range miss above", "0-30 * * * *", at(6, 18, 12, 31), false},
		{"list hit", "0,15,45 * * * *", at(6, 18, 12, 15), true},
		{"list miss", "0,15,45 * * * *", at(6, 18, 12, 30), false},
		{"step hit", "*/10 * * * *", at(6, 18, 12, 20), true},
		{"step miss", "*/10 * * * *", at(6, 18, 12, 25), false},
		{"step from base 0", "*/10 * * * *", at(6, 18, 12, 0), true},
		{"stepped range hit", "0-20/5 * * * *", at(6, 18, 12, 15), true},
		{"stepped range miss outside", "0-20/5 * * * *", at(6, 18, 12, 25), false},
		{"combo list+step", "1,2,4-6,*/10 * * * *", at(6, 18, 12, 4), true},
		{"combo step branch", "1,2,4-6,*/10 * * * *", at(6, 18, 12, 30), true},
		{"combo miss", "1,2,4-6,*/10 * * * *", at(6, 18, 12, 7), false},
		{"month match", "* * * 6 *", at(6, 18, 12, 0), true},
		{"month miss", "* * * 6 *", at(7, 18, 12, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, err := schedule.Parse(tt.spec)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.spec, err)
			}
			if got := sched.Matches(tt.t); got != tt.want {
				t.Fatalf("Parse(%q).Matches(%v) = %v, want %v", tt.spec, tt.t, got, tt.want)
			}
		})
	}
}

// TestValueStep pins the Vixie-cron "n/step" semantics: a bare value carrying a
// step expands to "n through the field maximum, stepping by step" — distinct from
// the single value n. Each case asserts the exact set of matched values for the
// stepped field (every value in [fieldMin,fieldMax]) across all five fields,
// including the boundaries 0/15 (from min) and 59/30 (where n is the field max, so
// the set collapses to {n}).
func TestValueStep(t *testing.T) {
	tests := []struct {
		name     string
		field    int    // 0=minute 1=hour 2=dom 3=month 4=dow
		spec     string // full five-field spec with the stepped term in `field`
		matched  []int  // values the stepped field must match
		fieldMax int    // upper bound of the field, for the exhaustive complement check
		fieldMin int    // lower bound of the field
	}{
		{"minute 5/15", 0, "5/15 * * * *", []int{5, 20, 35, 50}, 59, 0},
		{"minute 0/15", 0, "0/15 * * * *", []int{0, 15, 30, 45}, 59, 0},
		{"minute 59/30 collapses to n", 0, "59/30 * * * *", []int{59}, 59, 0},
		{"hour 1/6", 1, "* 1/6 * * *", []int{1, 7, 13, 19}, 23, 0},
		{"hour 0/12", 1, "* 0/12 * * *", []int{0, 12}, 23, 0},
		{"dom 1/10", 2, "* * 1/10 * *", []int{1, 11, 21, 31}, 31, 1},
		{"month 1/4", 3, "* * * 1/4 *", []int{1, 5, 9}, 12, 1},
		{"dow 0/2", 4, "* * * * 0/2", []int{0, 2, 4, 6}, 6, 0},
		{"dow 1/3", 4, "* * * * 1/3", []int{1, 4}, 6, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, err := schedule.Parse(tt.spec)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.spec, err)
			}
			want := map[int]bool{}
			for _, v := range tt.matched {
				want[v] = true
			}
			// Exhaustively check every value in the field's range: the listed values
			// match and all others do not. fieldMatches probes the stepped field while
			// holding the rest at a value the spec accepts.
			for v := tt.fieldMin; v <= tt.fieldMax; v++ {
				got := fieldMatches(sched, tt.field, v)
				if got != want[v] {
					t.Errorf("field %d value %d: matched=%v, want %v", tt.field, v, got, want[v])
				}
			}
		})
	}
}

// fieldMatches reports whether the stepped field index matches value v, holding
// the other four fields at a fixed valid value (the stepped specs above leave the
// other fields as "*", so any in-range time satisfies them). 2026 is used so the
// constructed times are valid; weekday cases pin specific dates.
func fieldMatches(s schedule.Schedule, field, v int) bool {
	// Base time: 2026-06-21 is a Sunday (weekday 0), the 21st, in June.
	month, day, hour, minute := time.June, 21, 9, 0
	switch field {
	case 0:
		minute = v
	case 1:
		hour = v
	case 2:
		// January has 31 days, so probing dom=1..31 never overflows into the next
		// month (June, the default, only has 30).
		month = time.January
		day = v
	case 3:
		month = time.Month(v)
		day = 1 // day 1 is valid in every month
	case 4:
		// Map weekday v to a concrete June 2026 date: 2026-06-21 is a Sunday, so
		// 21+v lands on weekday v for v in 0..6.
		day = 21 + v
	}
	return s.Matches(time.Date(atYear, month, day, hour, minute, 0, 0, time.UTC))
}

// TestDayOfWeek pins the 0=Sunday convention and the weekday matching. 2026-06-18
// is a Thursday (weekday 4); 2026-06-21 is a Sunday (weekday 0).
func TestDayOfWeek(t *testing.T) {
	thursday := at(6, 18, 9, 0) // weekday 4
	sunday := at(6, 21, 9, 0)   // weekday 0

	tests := []struct {
		spec string
		t    time.Time
		want bool
	}{
		{"* * * * 4", thursday, true},
		{"* * * * 4", sunday, false},
		{"* * * * 0", sunday, true},
		{"* * * * 0", thursday, false},
		{"* * * * 1-5", thursday, true}, // weekday range Mon-Fri includes Thu
		{"* * * * 1-5", sunday, false},  // Sunday excluded
		{"* * * * 0,6", sunday, true},   // weekend list includes Sunday
	}
	for _, tt := range tests {
		sched, err := schedule.Parse(tt.spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.spec, err)
		}
		if got := sched.Matches(tt.t); got != tt.want {
			t.Errorf("Parse(%q).Matches(%v) = %v, want %v", tt.spec, tt.t, got, tt.want)
		}
	}
}

// TestDayOfMonthAndWeekAndSemantics pins the documented AND-semantics: when BOTH
// the day-of-month and day-of-week fields are restricted, a time matches only when
// BOTH match (standard cron would OR them). 2026-06-18 is a Thursday (weekday 4).
func TestDayOfMonthAndWeekAndSemantics(t *testing.T) {
	// dom=18 AND dow=4(Thu): 2026-06-18 satisfies both.
	bothMatch, err := schedule.Parse("0 9 18 * 4")
	if err != nil {
		t.Fatal(err)
	}
	if !bothMatch.Matches(at(6, 18, 9, 0)) {
		t.Error("dom=18 dow=Thu should match 2026-06-18 09:00 (both match)")
	}

	// dom=18 AND dow=1(Mon): 2026-06-18 is the 18th but a Thursday, not Monday.
	// Standard cron OR-semantics would fire (dom matches); AND-semantics must NOT.
	domOnly, err := schedule.Parse("0 9 18 * 1")
	if err != nil {
		t.Fatal(err)
	}
	if domOnly.Matches(at(6, 18, 9, 0)) {
		t.Error("dom=18 dow=Mon must NOT match a Thursday-the-18th under AND-semantics")
	}

	// dom=19 AND dow=4(Thu): 2026-06-18 is a Thursday but not the 19th. AND must not
	// fire even though the weekday matches.
	dowOnly, err := schedule.Parse("0 9 19 * 4")
	if err != nil {
		t.Fatal(err)
	}
	if dowOnly.Matches(at(6, 18, 9, 0)) {
		t.Error("dom=19 dow=Thu must NOT match the 18th under AND-semantics")
	}
}
