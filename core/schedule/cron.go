package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronFields is the number of whitespace-separated fields a cron spec must have:
// minute, hour, day-of-month, month, day-of-week.
const cronFields = 5

// fieldBounds is the inclusive [min,max] valid range for each of the five cron
// fields, indexed by position. Day-of-week uses 0-6 with 0=Sunday (7 is NOT
// accepted as an alias for Sunday — keep the surface minimal and predictable).
var fieldBounds = [cronFields][2]int{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day-of-month
	{1, 12}, // month
	{0, 6},  // day-of-week (0=Sunday)
}

// fieldNames labels each field for error messages.
var fieldNames = [cronFields]string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// Schedule is a parsed five-field cron specification. Each field is precomputed
// into a set of the concrete integer values that satisfy it, so Matches is a few
// map lookups with no re-parsing.
//
// Day-of-month and day-of-week are combined with AND-semantics like every other
// field (NOT the standard cron OR-when-both-restricted rule): a job whose spec
// restricts BOTH dom and dow fires only on a day that satisfies both. This is a
// deliberate simplification for predictability and is documented in the package
// doc — callers should restrict at most one of the two day fields.
type Schedule struct {
	minute     map[int]bool
	hour       map[int]bool
	dayOfMonth map[int]bool
	month      map[int]bool
	dayOfWeek  map[int]bool
}

// Parse parses a five-field cron spec (minute hour day-of-month month
// day-of-week). Each field supports "*", a single value "n", an inclusive range
// "a-b", a comma list "a,b,c", and a step "*/n", "a-b/n", or "n/step", and any
// comma-combination of those (e.g. "1,2,4-6,*/10"). A bare value with a step,
// "n/step", means "n through the field maximum, stepping by step" (e.g. minute
// "5/15" -> {5,20,35,50}, "0/15" -> {0,15,30,45}), matching Vixie cron. Values
// are validated against each field's range at parse time, so a malformed or
// out-of-range spec returns an error and never persists.
func Parse(spec string) (Schedule, error) {
	parts := strings.Fields(strings.TrimSpace(spec))
	if len(parts) != cronFields {
		return Schedule{}, fmt.Errorf("cron spec must have %d fields, got %d", cronFields, len(parts))
	}
	sets := [cronFields]map[int]bool{}
	for i := 0; i < cronFields; i++ {
		set, err := parseField(parts[i], fieldBounds[i][0], fieldBounds[i][1])
		if err != nil {
			return Schedule{}, fmt.Errorf("%s field %q: %w", fieldNames[i], parts[i], err)
		}
		sets[i] = set
	}
	return Schedule{
		minute:     sets[0],
		hour:       sets[1],
		dayOfMonth: sets[2],
		month:      sets[3],
		dayOfWeek:  sets[4],
	}, nil
}

// parseField parses one comma-separated cron field into the set of integer values
// it matches, validating every value against [min,max]. It supports "*", "n",
// "a-b", "*/n", "a-b/n", and "n/step" terms.
func parseField(field string, minVal, maxVal int) (map[int]bool, error) {
	if field == "" {
		return nil, errors.New("empty field")
	}
	out := map[int]bool{}
	for _, term := range strings.Split(field, ",") {
		if term == "" {
			return nil, errors.New("empty list element")
		}
		if err := parseTerm(term, minVal, maxVal, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseTerm parses one comma-list element (a "*", value, range, or stepped
// variant) and adds every value it matches to out. A bare value carrying a step,
// "n/step", expands to "n through the field maximum, stepping by step" (Vixie
// cron semantics) rather than to the single value n.
func parseTerm(term string, minVal, maxVal int, out map[int]bool) error {
	rangePart := term
	step := 1
	hasStep := false
	if slash := strings.IndexByte(term, '/'); slash >= 0 {
		rangePart = term[:slash]
		stepStr := term[slash+1:]
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid step %q", stepStr)
		}
		step = n
		hasStep = true
	}

	lo, hi, single, err := termRange(rangePart, minVal, maxVal)
	if err != nil {
		return err
	}
	// "n/step" (a bare single value with a step) means "n through the field
	// maximum", so extend the upper bound to maxVal. "*/n" and "a-b/n" already
	// span an explicit range and keep their existing bounds.
	if hasStep && single {
		hi = maxVal
	}
	for v := lo; v <= hi; v += step {
		out[v] = true
	}
	return nil
}

// termRange resolves the [lo,hi] inclusive bounds a term's range part covers: "*"
// spans the whole field, "n" is the single value n (single=true), and "a-b" is
// the explicit range. Every endpoint is validated against [minVal,maxVal] and
// lo<=hi is enforced.
func termRange(rangePart string, minVal, maxVal int) (lo, hi int, single bool, err error) {
	if rangePart == "*" {
		return minVal, maxVal, false, nil
	}
	if dash := strings.IndexByte(rangePart, '-'); dash >= 0 {
		lo, err = parseBoundedInt(rangePart[:dash], minVal, maxVal)
		if err != nil {
			return 0, 0, false, err
		}
		hi, err = parseBoundedInt(rangePart[dash+1:], minVal, maxVal)
		if err != nil {
			return 0, 0, false, err
		}
		if lo > hi {
			return 0, 0, false, fmt.Errorf("range %q is descending", rangePart)
		}
		return lo, hi, false, nil
	}
	v, err := parseBoundedInt(rangePart, minVal, maxVal)
	if err != nil {
		return 0, 0, false, err
	}
	return v, v, true, nil
}

// parseBoundedInt parses s as an integer and verifies it lies within
// [minVal,maxVal], returning a clear error otherwise.
func parseBoundedInt(s string, minVal, maxVal int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	if v < minVal || v > maxVal {
		return 0, fmt.Errorf("value %d out of range [%d,%d]", v, minVal, maxVal)
	}
	return v, nil
}

// Matches reports whether t satisfies the schedule. All five fields are combined
// with AND-semantics — including day-of-month and day-of-week (see the Schedule
// doc) — so the time matches only when every field matches. t is evaluated in its
// own location (the Manager passes process-local time), at minute granularity.
func (s Schedule) Matches(t time.Time) bool {
	weekday := int(t.Weekday()) // time.Sunday == 0, matching the dow convention
	return s.minute[t.Minute()] &&
		s.hour[t.Hour()] &&
		s.dayOfMonth[t.Day()] &&
		s.month[int(t.Month())] &&
		s.dayOfWeek[weekday]
}
