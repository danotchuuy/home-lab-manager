package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression: minute, hour, day of month,
// month and day of week.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit sets
	// domAny and dowAny are set when the field was "*": as in cron, when
	// both day fields are restricted, a day matching either one counts.
	domAny, dowAny bool
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	fields   = []field{
		{"minute", 0, 59, nil},
		{"hour", 0, 23, nil},
		{"day of month", 1, 31, nil},
		{"month", 1, 12, monthNames},
		{"day of week", 0, 7, dayNames}, // 7 is Sunday too
	}
)

// ParseCron parses a cron expression like "30 4 * * 1-5". Each field takes
// *, numbers, ranges (1-5), steps (*/15, 0-30/10), lists (1,15) and, for
// months and days, names (jan, mon). Shortcuts like @daily are accepted too.
func ParseCron(expr string) (Cron, error) {
	expr = strings.TrimSpace(expr)
	switch strings.ToLower(expr) {
	case "@hourly":
		expr = "0 * * * *"
	case "@daily", "@midnight":
		expr = "0 0 * * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@monthly":
		expr = "0 0 1 * *"
	case "@yearly", "@annually":
		expr = "0 0 1 1 *"
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Cron{}, errors.New("a cron expression has five fields: minute hour day-of-month month day-of-week")
	}
	var c Cron
	sets := []*uint64{&c.minute, &c.hour, &c.dom, &c.month, &c.dow}
	for i, p := range parts {
		set, err := parseField(p, fields[i])
		if err != nil {
			return Cron{}, err
		}
		*sets[i] = set
	}
	if c.dow&(1<<7) != 0 {
		c.dow |= 1 // 7 is Sunday
	}
	c.domAny, c.dowAny = parts[2] == "*" || parts[2] == "?", parts[4] == "*" || parts[4] == "?"
	return c, nil
}

func parseField(s string, f field) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(s, ",") {
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step %q in the %s field", stepText, f.name)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = value(a, f); err != nil {
				return 0, err
			}
			if hi, err = value(b, f); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("the range %q in the %s field runs backwards", rng, f.name)
			}
		default:
			v, err := value(rng, f)
			if err != nil {
				return 0, err
			}
			lo = v
			if !hasStep {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func value(s string, f field) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%q isn't a valid %s (%d to %d)", s, f.name, f.min, f.max)
	}
	return v, nil
}

func has(set uint64, v int) bool { return set&(1<<v) != 0 }

func (c Cron) dayMatches(t time.Time) bool {
	dom, dow := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// Next returns the first time after after, in loc, that the expression
// matches. A time that doesn't exist that day, because the clocks went
// forward, runs when they did (02:30 runs at 03:00); one that happens twice
// runs once. It returns the zero time if nothing matches within five years
// (such as 31 February).
func (c Cron) Next(after time.Time, loc *time.Location) time.Time {
	after = after.In(loc)
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
	for range 366 * 5 {
		if has(c.month, int(day.Month())) && c.dayMatches(day) {
			for h := range 24 {
				if !has(c.hour, h) {
					continue
				}
				for m := range 60 {
					if !has(c.minute, m) {
						continue
					}
					t := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
					if t.Hour() != h || t.Minute() != m {
						// Skipped by the clocks going forward: run when they did.
						t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
					}
					if t.After(after) {
						return t
					}
				}
			}
		}
		day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
	}
	return time.Time{}
}
