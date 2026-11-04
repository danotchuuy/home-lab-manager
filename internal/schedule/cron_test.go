package schedule

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestParseCronErrors(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "* * * foo *", "1-x * * * *",
	} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("ParseCron(%q) accepted", bad)
		}
	}
}

func TestCronNext(t *testing.T) {
	utc := time.UTC
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, utc)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	tests := []struct {
		expr, after, want string
	}{
		{"30 4 * * *", "2026-11-04 12:00", "2026-11-05 04:30"},
		{"30 4 * * *", "2026-11-04 04:29", "2026-11-04 04:30"},
		{"30 4 * * *", "2026-11-04 04:30", "2026-11-05 04:30"}, // strictly after
		{"*/15 * * * *", "2026-11-04 12:07", "2026-11-04 12:15"},
		{"*/15 * * * *", "2026-11-04 23:50", "2026-11-05 00:00"},
		{"5/20 * * * *", "2026-11-04 12:26", "2026-11-04 12:45"},
		{"0 9-17/4 * * *", "2026-11-04 10:00", "2026-11-04 13:00"},
		{"0 0 * * mon-fri", "2026-11-06 12:00", "2026-11-09 00:00"}, // Friday → Monday
		{"0 0 * * 0", "2026-11-04 00:00", "2026-11-08 00:00"},
		{"0 0 * * 7", "2026-11-04 00:00", "2026-11-08 00:00"}, // 7 is Sunday too
		{"0 0 1,15 * *", "2026-11-02 00:00", "2026-11-15 00:00"},
		{"0 0 31 * *", "2026-11-01 00:00", "2026-12-31 00:00"}, // November has no 31st
		{"0 0 29 feb *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 1 jan,jul *", "2026-11-04 00:00", "2027-01-01 12:00"},
		// Both day fields restricted: either matches (13th, or any Friday).
		{"0 0 13 * fri", "2026-11-04 00:00", "2026-11-06 00:00"},
		{"0 0 13 * fri", "2026-11-07 00:00", "2026-11-13 00:00"},
		{"@daily", "2026-11-04 12:00", "2026-11-05 00:00"},
		{"@hourly", "2026-11-04 12:00", "2026-11-04 13:00"},
	}
	for _, tt := range tests {
		c, err := ParseCron(tt.expr)
		if err != nil {
			t.Errorf("ParseCron(%q): %v", tt.expr, err)
			continue
		}
		if got := c.Next(at(tt.after), utc); !got.Equal(at(tt.want)) {
			t.Errorf("%q after %s = %s, want %s", tt.expr, tt.after, got.Format("2006-01-02 15:04 Mon"), tt.want)
		}
	}
	c, _ := ParseCron("0 0 31 2 *")
	if got := c.Next(at("2026-01-01 00:00"), utc); !got.IsZero() {
		t.Errorf("31 February = %v, want never", got)
	}
}

func TestCronDST(t *testing.T) {
	london := mustLoad(t, "Europe/London")
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, london)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	// 29 March 2026: clocks go forward at 01:00 to 02:00, so 01:30 doesn't
	// exist; it runs at 02:00 BST.
	c, _ := ParseCron("30 1 * * *")
	got := c.Next(at("2026-03-28 12:00"), london)
	if got.In(london).Format("2006-01-02 15:04 MST") != "2026-03-29 02:00 BST" {
		t.Errorf("in the gap: %s", got.In(london).Format("2006-01-02 15:04 MST"))
	}
	if next := c.Next(got, london); next.In(london).Format("2006-01-02 15:04 MST") != "2026-03-30 01:30 BST" {
		t.Errorf("the day after the gap: %s", next.In(london).Format("2006-01-02 15:04 MST"))
	}
	// Every 15 minutes across the gap: 00:45, then 02:00 (01:00-01:45 all
	// map to 02:00 and run once), then 02:15.
	q, _ := ParseCron("*/15 * * * *")
	t1 := q.Next(at("2026-03-29 00:40"), london)
	t2 := q.Next(t1, london)
	t3 := q.Next(t2, london)
	var got3 []string
	for _, x := range []time.Time{t1, t2, t3} {
		got3 = append(got3, x.In(london).Format("15:04 MST"))
	}
	if got3[0] != "00:45 GMT" || got3[1] != "02:00 BST" || got3[2] != "02:15 BST" {
		t.Errorf("every 15 minutes across the gap: %v", got3)
	}

	// 25 October 2026: clocks go back at 02:00 to 01:00, so 01:30 happens
	// twice; it runs once.
	first := c.Next(at("2026-10-24 12:00"), london)
	second := c.Next(first, london)
	if first.In(london).Format("2006-01-02 15:04") != "2026-10-25 01:30" ||
		second.In(london).Format("2006-01-02 15:04") != "2026-10-26 01:30" {
		t.Errorf("repeated hour: %s then %s", first.In(london), second.In(london))
	}
}
