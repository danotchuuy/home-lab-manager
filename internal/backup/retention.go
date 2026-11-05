package backup

import (
	"errors"
	"fmt"
	"time"
)

// Retention keeps backups by age (grandfather-father-son): the newest Last,
// and the newest of each of the most recent Daily days, Weekly weeks and
// Monthly months that have a backup. A backup any rule keeps is kept.
type Retention struct {
	Last    int `json:"last"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// DefaultRetention is offered when switching a plan to keeping by age.
var DefaultRetention = Retention{Last: 3, Daily: 7, Weekly: 4, Monthly: 6}

func (r Retention) check() error {
	for _, n := range []int{r.Last, r.Daily, r.Weekly, r.Monthly} {
		if n < 0 || n > 1000 {
			return errors.New("each number of backups to keep is 0 to 1000")
		}
	}
	if r.Last+r.Daily+r.Weekly+r.Monthly == 0 {
		return errors.New("keep at least one backup")
	}
	return nil
}

// Keeping is how a plan keeps backups: Retention if set, or else the
// newest Keep (0: all).
type Keeping struct {
	Keep      int        `json:"keep"`
	Retention *Retention `json:"retention,omitempty"`
}

func (k Keeping) check() error {
	if k.Keep < 0 {
		return errors.New("the number of backups to keep can't be negative")
	}
	if k.Retention != nil {
		return k.Retention.check()
	}
	return nil
}

// prunes reports whether k ever deletes backups.
func (k Keeping) prunes() bool { return k.Retention != nil || k.Keep > 0 }

// keepSet returns which of the backups taken at times (newest first) k
// keeps, as a slice of the same length.
func (k Keeping) keepSet(times []time.Time, loc *time.Location) []bool {
	keep := make([]bool, len(times))
	if k.Retention == nil {
		for i := range times {
			keep[i] = k.Keep <= 0 || i < k.Keep
		}
		return keep
	}
	r := *k.Retention
	for i := 0; i < len(times) && i < r.Last; i++ {
		keep[i] = true
	}
	// The newest backup in each period, for the most recent n periods.
	byPeriod := func(n int, period func(time.Time) string) {
		seen := map[string]bool{}
		for i, t := range times {
			if len(seen) >= n {
				return
			}
			p := period(t.In(loc))
			if !seen[p] {
				seen[p] = true
				keep[i] = true
			}
		}
	}
	byPeriod(r.Daily, func(t time.Time) string { return t.Format("2006-01-02") })
	byPeriod(r.Weekly, func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	})
	byPeriod(r.Monthly, func(t time.Time) string { return t.Format("2006-01") })
	return keep
}

// keyTime reads the UTC time at the end of a backup's key.
func keyTime(key string) (time.Time, bool) {
	n := len(timeLayout + ".tar.gz")
	if len(key) < n {
		return time.Time{}, false
	}
	t, err := time.Parse(timeLayout, key[len(key)-n:len(key)-len(".tar.gz")])
	return t, err == nil
}
