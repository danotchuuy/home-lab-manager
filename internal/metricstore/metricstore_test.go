package metricstore

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openAt(t *testing.T, dir string, now *time.Time) *Store {
	t.Helper()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return *now }
	return s
}

func val(n Num) float64 { return float64(n) }

func TestMinutesAndQuarters(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	s := openAt(t, t.TempDir(), &now)
	// 20 minutes of samples every 15 s: value = minute index, peak at :30.
	for i := range 20 * 4 {
		ts := now.Add(time.Duration(i) * 15 * time.Second)
		v := float64(i / 4)
		if i%4 == 2 {
			v += 10
		}
		s.Add("host", "cpu", ts, v)
	}
	now = now.Add(20 * time.Minute) // 12:20, nothing sampled yet this minute

	h := s.Query("host", time.Hour)
	if h.Step != 60 || len(h.Times) != 60 {
		t.Fatalf("step %d, %d times", h.Step, len(h.Times))
	}
	cpu := h.Series["cpu"]
	// The last 21 steps are 12:00 … 12:20; 12:20 has no samples.
	at := func(m int) int { return len(h.Times) - 21 + m }
	if got := time.UnixMilli(h.Times[at(0)]).UTC(); !got.Equal(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("12:00 is at %v", got)
	}
	for m := range 20 {
		if a, mx := val(cpu.Avg[at(m)]), val(cpu.Max[at(m)]); a != float64(m)+2.5 || mx != float64(m)+10 {
			t.Errorf("minute %d: avg %v max %v", m, a, mx)
		}
	}
	if !math.IsNaN(val(cpu.Avg[at(20)])) || !math.IsNaN(val(cpu.Avg[0])) {
		t.Error("empty minutes should be missing")
	}

	// 7 days: 15-minute steps. 12:00–12:15 averages minutes 0–14.
	w := s.Query("host", 7*24*time.Hour)
	if w.Step != 900 || len(w.Times) != 7*24*4 {
		t.Fatalf("week: step %d, %d times", w.Step, len(w.Times))
	}
	q := w.Series["cpu"]
	n := len(w.Times)
	if a, mx := val(q.Avg[n-2]), val(q.Max[n-2]); a != 7+2.5 || mx != 14+10 {
		t.Errorf("12:00 quarter: avg %v max %v", a, mx)
	}
	if a := val(q.Avg[n-1]); a != 17+2.5 { // 12:15–12:20, minutes 15–19
		t.Errorf("12:15 quarter: avg %v", a)
	}

	// JSON: missing values are null.
	b, _ := json.Marshal(cpu)
	if !strings.HasPrefix(string(b), `{"avg":[null,`) || !strings.Contains(string(b), "2.5,3.5") {
		t.Errorf("json %.80s", b)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	s := openAt(t, dir, &now)
	for m := range 3 {
		s.Add("servers/valheim", "mem", now.Add(time.Duration(m)*time.Minute), float64(100*(m+1)))
	}
	now = now.Add(3 * time.Minute)
	if err := s.Flush(); err != nil { // minutes 0–2 go to the log
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, logFile)); err != nil {
		t.Fatal(err)
	}

	// Reopened from the log alone.
	r := openAt(t, dir, &now)
	mem := r.Query("servers/valheim", time.Hour).Series["mem"]
	if got := val(mem.Avg[len(mem.Avg)-2]); got != 300 {
		t.Errorf("after replay, last minute = %v", got)
	}

	// Compacted: a snapshot, no log; then more minutes in a new log.
	if err := r.Compact(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, logFile)); !os.IsNotExist(err) {
		t.Errorf("log after compact: %v", err)
	}
	r.Add("servers/valheim", "mem", now, 400)
	now = now.Add(time.Minute)
	r.Flush()

	r2 := openAt(t, dir, &now)
	mem = r2.Query("servers/valheim", time.Hour).Series["mem"]
	n := len(mem.Avg)
	if val(mem.Avg[n-5]) != 100 || val(mem.Avg[n-2]) != 400 {
		t.Errorf("after snapshot + log: %v", mem.Avg[n-6:])
	}
	// The 15-minute bucket isn't counted twice.
	if q := r2.Query("servers/valheim", 7*24*time.Hour).Series["mem"]; val(q.Avg[len(q.Avg)-1]) != 250 {
		t.Errorf("quarter avg = %v, want 250", q.Avg[len(q.Avg)-1])
	}
}

func TestDamageAndRetention(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, snapshotFile), []byte("not gob"), 0o600)
	os.WriteFile(filepath.Join(dir, logFile), []byte("{\"g\":\"host\",\"s\":\"cpu\",\"t\":1793793600,\"sum\":5,\"max\":5,\"n\":1}\n{broken\n"), 0o600)
	now := time.Unix(1793793600, 0).Add(2 * time.Minute)
	s := openAt(t, dir, &now)
	if _, err := os.Stat(filepath.Join(dir, snapshotFile+".bad")); err != nil {
		t.Errorf("damaged snapshot not set aside: %v", err)
	}
	if cpu := s.Query("host", time.Hour).Series["cpu"]; val(cpu.Avg[len(cpu.Avg)-3]) != 5 {
		t.Errorf("good log line lost: %v", cpu.Avg[len(cpu.Avg)-3:])
	}

	s.Add("servers/old", "cpu", now, 1)
	s.Add("host", "cpu", now, 1)
	now = now.Add(Retention + time.Hour)
	s.Add("host", "cpu", now, 2)
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	if g := s.Groups(); len(g) != 1 || g[0] != "host" {
		t.Errorf("groups after retention: %v", g)
	}
	s.Add("host", "cpu", now, math.NaN()) // ignored
	var nilStore *Store
	nilStore.Add("x", "y", now, 1)
}
