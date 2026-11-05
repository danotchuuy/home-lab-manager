// Package metricstore keeps a month of resource history (host and servers)
// in fixed-size rings, without an external database.
//
// Samples are averaged (and their peak kept) per minute; finished minutes go
// into a ring of the last 24 hours and are merged into a ring of 15-minute
// buckets covering 30 days. Finished minutes are appended to a small log as
// they finish, and the whole store is written out every few hours and on
// shutdown, so a restart loses at most the minute in progress, and the disk
// sees a few kilobytes a minute rather than the whole store.
package metricstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Tier is a resolution and how many buckets of it are kept.
type Tier struct {
	Step time.Duration
	Len  int
}

// Tiers are the kept resolutions: a minute for a day, 15 minutes for 30
// days.
var Tiers = [2]Tier{{time.Minute, 24 * 60}, {15 * time.Minute, 30 * 24 * 4}}

const (
	snapshotFile = "snapshot.gob"
	logFile      = "minutes.jsonl"
	// compactEvery is how often the store is written out whole and the log
	// started again.
	compactEvery = 6 * time.Hour
	// Retention is how long a group (a deleted server's history) is kept
	// after its last sample.
	Retention = 30 * 24 * time.Hour
)

// bucket is the samples of one step: their sum, count and peak.
type bucket struct {
	T   int64 // start, unix seconds
	Sum float64
	Max float32
	N   uint32
}

func (b *bucket) add(v float64) {
	if b.N == 0 || float32(v) > b.Max {
		b.Max = float32(v)
	}
	b.Sum += v
	b.N++
}

func (b *bucket) merge(o bucket) {
	if b.N == 0 || o.Max > b.Max {
		b.Max = o.Max
	}
	b.Sum += o.Sum
	b.N += o.N
}

// series is one measurement's rings, one per tier, allocated when first
// written.
type series struct {
	Rings [2][]bucket
	acc   bucket // the minute in progress
}

func (s *series) put(tier int, b bucket) {
	t := Tiers[tier]
	if s.Rings[tier] == nil {
		s.Rings[tier] = make([]bucket, t.Len)
	}
	step := int64(t.Step / time.Second)
	start := b.T - b.T%step
	slot := &s.Rings[tier][(start/step)%int64(t.Len)]
	if tier == 0 {
		*slot = b
		return
	}
	if slot.T != start {
		*slot = bucket{T: start}
	}
	slot.merge(b)
}

// group is one host's or server's series, by name.
type group map[string]*series

// Store is the history.
type Store struct {
	dir string
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	groups  map[string]group
	pending []logRecord // finished minutes not yet appended to the log
}

// logRecord is a finished minute, as a line of the log.
type logRecord struct {
	G   string  `json:"g"`
	S   string  `json:"s"`
	T   int64   `json:"t"`
	Sum float64 `json:"sum"`
	Max float32 `json:"max"`
	N   uint32  `json:"n"`
}

// Open loads the history kept in dir. A damaged snapshot or log is set
// aside, not fatal: history starts again.
func Open(dir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, log: log, now: time.Now, groups: map[string]group{}}
	if err := s.loadSnapshot(); err != nil {
		log.Warn("metrics history: snapshot unreadable, starting again", "err", err)
		_ = os.Rename(filepath.Join(dir, snapshotFile), filepath.Join(dir, snapshotFile+".bad"))
		s.groups = map[string]group{}
	}
	if n, bad, err := s.replayLog(); err != nil {
		log.Warn("metrics history: log unreadable", "err", err)
	} else if bad > 0 {
		log.Warn("metrics history: skipped damaged log lines", "lines", bad, "read", n)
	}
	return s, nil
}

func (s *Store) loadSnapshot() error {
	f, err := os.Open(filepath.Join(s.dir, snapshotFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var snap map[string]map[string]*series
	if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&snap); err != nil {
		return err
	}
	for g, ss := range snap {
		for name, sr := range ss {
			for i, ring := range sr.Rings {
				if ring != nil && len(ring) != Tiers[i].Len {
					return fmt.Errorf("series %s/%s: ring %d has %d buckets, want %d", g, name, i, len(ring), Tiers[i].Len)
				}
			}
		}
		s.groups[g] = group(ss)
	}
	return nil
}

func (s *Store) replayLog() (n, bad int, err error) {
	f, err := os.Open(filepath.Join(s.dir, logFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r logRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.G == "" || r.S == "" || r.N == 0 {
			bad++
			continue
		}
		s.finish(r.G, r.S, bucket{T: r.T, Sum: r.Sum, Max: r.Max, N: r.N}, false)
		n++
	}
	return n, bad, sc.Err()
}

func (s *Store) seriesOf(g, name string) *series {
	gr := s.groups[g]
	if gr == nil {
		gr = group{}
		s.groups[g] = gr
	}
	sr := gr[name]
	if sr == nil {
		sr = &series{}
		gr[name] = sr
	}
	return sr
}

// finish puts a finished minute in the rings (and, if record, the log).
func (s *Store) finish(g, name string, b bucket, record bool) {
	sr := s.seriesOf(g, name)
	sr.put(0, b)
	sr.put(1, b)
	if record {
		s.pending = append(s.pending, logRecord{G: g, S: name, T: b.T, Sum: b.Sum, Max: b.Max, N: b.N})
	}
}

// Add records a sample of a group's series (e.g. "host", "cpu"). NaN and
// infinite values are ignored.
func (s *Store) Add(g, name string, t time.Time, v float64) {
	if s == nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	minute := t.Unix() - t.Unix()%60
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := s.seriesOf(g, name)
	if sr.acc.N > 0 && sr.acc.T != minute {
		if minute < sr.acc.T {
			return // out of order: dropped
		}
		s.finish(g, name, sr.acc, true)
		sr.acc = bucket{}
	}
	sr.acc.T = minute
	sr.acc.add(v)
}

// tick finishes minutes that are over, for series no longer sampled.
func (s *Store) tick(now time.Time) {
	minute := now.Unix() - now.Unix()%60
	for g, gr := range s.groups {
		for name, sr := range gr {
			if sr.acc.N > 0 && sr.acc.T < minute {
				s.finish(g, name, sr.acc, true)
				sr.acc = bucket{}
			}
		}
	}
}

// Flush finishes past minutes and appends them to the log.
func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick(s.now())
	return s.appendPending()
}

func (s *Store) appendPending() error {
	if len(s.pending) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range s.pending {
		_ = enc.Encode(r)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, logFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.pending = s.pending[:0]
	return nil
}

// Compact drops groups with nothing recent, writes the whole store out and
// starts the log again.
func (s *Store) Compact() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.tick(now)
	cutoff := now.Add(-Retention).Unix()
	for g, gr := range s.groups {
		if latest(gr) < cutoff {
			delete(s.groups, g)
		}
	}
	tmp, err := os.CreateTemp(s.dir, snapshotFile+".*")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	snap := make(map[string]map[string]*series, len(s.groups))
	for g, gr := range s.groups {
		snap[g] = gr
	}
	err = gob.NewEncoder(w).Encode(snap)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(s.dir, snapshotFile))
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	// The snapshot has every finished minute, so the log starts empty.
	s.pending = s.pending[:0]
	if err := os.Remove(filepath.Join(s.dir, logFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// latest is the start of a group's newest bucket, unix seconds.
func latest(gr group) int64 {
	var t int64
	for _, sr := range gr {
		t = max(t, sr.acc.T)
		for _, ring := range sr.Rings {
			for _, b := range ring {
				if b.N > 0 {
					t = max(t, b.T)
				}
			}
		}
	}
	return t
}

// Run flushes every 15 seconds and compacts every few hours until ctx ends.
// Close saves the rest.
func (s *Store) Run(ctx context.Context) {
	flush := time.NewTicker(15 * time.Second)
	defer flush.Stop()
	compact := time.NewTicker(compactEvery)
	defer compact.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			if err := s.Flush(); err != nil {
				s.log.Warn("metrics history: append", "err", err)
			}
		case <-compact.C:
			if err := s.Compact(); err != nil {
				s.log.Warn("metrics history: save", "err", err)
			}
		}
	}
}

// Num is a value in a history: null in JSON when missing.
type Num float64

// MarshalJSON writes NaN (no samples) as null, and others to 6 significant
// digits.
func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, f, 'g', 6, 64), nil
}

// UnmarshalJSON reads null as NaN.
func (n *Num) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*n = Num(math.NaN())
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	*n = Num(f)
	return err
}

// Values are a series' averages and peaks, one per step.
type Values struct {
	Avg []Num `json:"avg"`
	Max []Num `json:"max"`
}

// History is a group's series over a range.
type History struct {
	Step   int64             `json:"step"`  // seconds
	Times  []int64           `json:"times"` // each step's start, unix milliseconds
	Series map[string]Values `json:"series"`
}

// Ranges are the ranges history can be asked for.
var Ranges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// Query returns a group's series over the last rng: per minute up to a day,
// per 15 minutes beyond. The minute in progress is included.
func (s *Store) Query(g string, rng time.Duration) History {
	if s == nil {
		return History{Times: []int64{}, Series: map[string]Values{}}
	}
	tier := 0
	if rng > Tiers[0].Step*time.Duration(Tiers[0].Len) {
		tier = 1
	}
	t := Tiers[tier]
	rng = min(rng, t.Step*time.Duration(t.Len))
	step := int64(t.Step / time.Second)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	last := now - now%step
	first := last - int64(rng/time.Second) + step
	n := int((last-first)/step) + 1
	h := History{Step: step, Times: make([]int64, n), Series: map[string]Values{}}
	for i := range n {
		h.Times[i] = (first + int64(i)*step) * 1000
	}
	gr := s.groups[g]
	for _, name := range sortedNames(gr) {
		sr := gr[name]
		v := Values{Avg: make([]Num, n), Max: make([]Num, n)}
		any := false
		for i := range n {
			start := first + int64(i)*step
			var b bucket
			if ring := sr.Rings[tier]; ring != nil {
				if slot := ring[(start/step)%int64(t.Len)]; slot.T == start {
					b = slot
				}
			}
			// The minute in progress isn't in the rings yet.
			if sr.acc.N > 0 && sr.acc.T-sr.acc.T%step == start {
				b.merge(sr.acc)
			}
			if b.N == 0 {
				v.Avg[i], v.Max[i] = Num(math.NaN()), Num(math.NaN())
				continue
			}
			any = true
			v.Avg[i], v.Max[i] = Num(b.Sum/float64(b.N)), Num(b.Max)
		}
		if any {
			h.Series[name] = v
		}
	}
	return h
}

func sortedNames(gr group) []string {
	names := make([]string, 0, len(gr))
	for n := range gr {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Groups lists the groups with history.
func (s *Store) Groups() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.groups))
	for g := range s.groups {
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}

// Close finishes and saves everything.
func (s *Store) Close() error { return s.Compact() }
