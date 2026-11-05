package backup

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// At backs up at a time of day, on chosen weekdays, in hlm's time zone.
type At struct {
	Time string `json:"time"` // "04:00"
	// Weekdays are 0 (Sunday) to 6; empty means every day.
	Weekdays []int `json:"weekdays,omitempty"`
}

func (a At) check() (At, error) {
	h, m, ok := clock(a.Time)
	if !ok {
		return a, errors.New("the time must look like 04:30")
	}
	a.Time = fmt.Sprintf("%02d:%02d", h, m)
	var days []int
	for _, d := range a.Weekdays {
		if d < 0 || d > 6 {
			return a, errors.New("weekdays are 0 (Sunday) to 6 (Saturday)")
		}
		if !slices.Contains(days, d) {
			days = append(days, d)
		}
	}
	slices.Sort(days)
	if len(days) == 7 {
		days = nil
	}
	a.Weekdays = days
	return a, nil
}

func clock(s string) (int, int, bool) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(s), ":")
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	return h, m, ok && err1 == nil && err2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60
}

// next returns the first time after after that the schedule comes due.
// When the clocks go forward past it, it runs when they did.
func (a At) next(after time.Time, loc *time.Location) time.Time {
	h, m, ok := clock(a.Time)
	if !ok {
		return time.Time{}
	}
	t := after.In(loc)
	for i := 0; i < 8; i++ {
		d := t.AddDate(0, 0, i)
		at := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if !at.After(after) {
			continue
		}
		if len(a.Weekdays) == 0 || slices.Contains(a.Weekdays, int(at.Weekday())) {
			return at
		}
	}
	return time.Time{}
}
