// Package audit keeps a record of who changed what in hlm: every change made
// through the API, sign-ins and sign-outs, and what hlm does by itself
// (scheduled tasks, automatic updates, scheduled backups).
//
// Entries are appended to one JSON Lines file a month in the data
// directory, and can't be changed or deleted through hlm; old months are
// deleted after the retention period.
package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Results.
const (
	ResultOK     = "ok"
	ResultDenied = "denied" // not signed in, or not allowed
	ResultError  = "error"  // allowed, but it failed
)

// Kinds of actor.
const (
	ActorLocal     = "local"     // a local account
	ActorSSO       = "sso"       // single sign-on
	ActorKey       = "key"       // an API key
	ActorSSH       = "ssh"       // a token from an SSH key sign-in
	ActorToken     = "token"     // HLM_TOKEN
	ActorAnonymous = "anonymous" // anyone, when sign-in isn't required
	ActorHLM       = "hlm"       // hlm itself: schedules and automation
	ActorHost      = "host"      // someone at the host, running hlm's commands
	ActorDiscord   = "discord"   // someone in Discord, through hlm's bot
)

// Entry is one thing that happened.
type Entry struct {
	Time      time.Time `json:"time"`
	Actor     string    `json:"actor"`
	ActorKind string    `json:"actorKind"`
	IP        string    `json:"ip,omitempty"`
	// Action is what was done, e.g. servers.restart, auth.login,
	// notifications.targets.create.
	Action string `json:"action"`
	// Target is what it was done to (a server, an account, a key…).
	Target string `json:"target,omitempty"`
	// Server is the server it was about, if any.
	Server  string            `json:"server,omitempty"`
	Result  string            `json:"result"`
	Status  int               `json:"status,omitempty"` // the HTTP status, for API requests
	Error   string            `json:"error,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// Recorder records entries. A nil one drops them.
type Recorder interface {
	Record(Entry)
}

// Settings are the log's settings.
type Settings struct {
	// RetentionMonths is how many months of entries are kept, the current
	// one included; 0 means the default.
	RetentionMonths int `json:"retentionMonths,omitempty"`
	// Mirror also writes each entry to hlm's own log, for collecting
	// elsewhere (journald, Loki…).
	Mirror bool `json:"mirror,omitempty"`
}

// DefaultRetention is the default RetentionMonths.
const DefaultRetention = 12

// Months returns RetentionMonths or its default.
func (s Settings) Months() int {
	if s.RetentionMonths <= 0 {
		return DefaultRetention
	}
	return s.RetentionMonths
}

const secretName = "audit"

// Log is the audit log.
type Log struct {
	dir     string
	secrets *secrets.Store
	log     *slog.Logger
	now     func() time.Time

	mu    sync.Mutex
	file  *os.File
	month string // of file
}

// Open opens (creating if needed) the log in dir.
func Open(dir string, store *secrets.Store, log *slog.Logger) (*Log, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Log{dir: dir, secrets: store, log: log, now: time.Now}, nil
}

// Settings returns the log's settings.
func (l *Log) Settings() Settings {
	var s Settings
	if l.secrets != nil {
		_, _ = l.secrets.Get(secretName, &s)
	}
	return s
}

// SetSettings saves the log's settings and applies the retention period.
func (l *Log) SetSettings(s Settings) error {
	if s.RetentionMonths < 0 || s.RetentionMonths > 120 {
		return errors.New("keep entries for 1 to 120 months")
	}
	if err := l.secrets.Set(secretName, s); err != nil {
		return err
	}
	return l.prune()
}

func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

func (l *Log) path(month string) string { return filepath.Join(l.dir, "audit-"+month+".jsonl") }

// clean redacts what errors and details sometimes carry.
func clean(e Entry) Entry {
	e.Error = notify.Redact(e.Error)
	if len(e.Error) > 1000 {
		e.Error = e.Error[:1000] + "…"
	}
	if len(e.Details) > 0 {
		d := make(map[string]string, len(e.Details))
		for k, v := range e.Details {
			v = notify.Redact(v)
			if len(v) > 2000 {
				v = v[:2000] + "…"
			}
			d[k] = v
		}
		e.Details = d
	}
	return e
}

// Record appends an entry. It never fails the caller: problems are logged.
func (l *Log) Record(e Entry) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	e.Time = e.Time.UTC()
	if e.Result == "" {
		e.Result = ResultOK
	}
	e = clean(e)
	data, err := json.Marshal(e)
	if err != nil {
		l.log.Warn("encode audit entry", "err", err)
		return
	}
	data = append(data, '\n')
	l.mu.Lock()
	err = l.write(e.Time, data)
	l.mu.Unlock()
	if err != nil {
		l.log.Error("write audit entry", "err", err, "action", e.Action, "actor", e.Actor)
	}
	if l.Settings().Mirror {
		l.log.Info("audit", "actor", e.Actor, "actorKind", e.ActorKind, "ip", e.IP, "action", e.Action,
			"target", e.Target, "result", e.Result, "error", e.Error)
	}
}

func (l *Log) write(t time.Time, data []byte) error {
	m := monthOf(t)
	if l.file == nil || l.month != m {
		if l.file != nil {
			l.file.Close()
		}
		f, err := os.OpenFile(l.path(m), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			l.file = nil
			return err
		}
		l.file, l.month = f, m
	}
	if _, err := l.file.Write(data); err != nil {
		return err
	}
	return l.file.Sync()
}

// Close closes the current file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// months lists the months with a file, newest first.
func (l *Log) months() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if m, ok := strings.CutPrefix(name, "audit-"); ok && strings.HasSuffix(m, ".jsonl") {
			m = strings.TrimSuffix(m, ".jsonl")
			if _, err := time.Parse("2006-01", m); err == nil {
				out = append(out, m)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// prune deletes the files of months older than the retention period.
func (l *Log) prune() error {
	keep := l.Settings().Months()
	now := l.now().UTC()
	oldest := monthOf(time.Date(now.Year(), now.Month()-time.Month(keep-1), 1, 0, 0, 0, 0, time.UTC))
	months, err := l.months()
	if err != nil {
		return err
	}
	for _, m := range months {
		if m < oldest {
			if err := os.Remove(l.path(m)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			l.log.Info("deleted old audit entries", "month", m)
		}
	}
	return nil
}

// Run applies the retention period now and every day, until ctx ends.
func (l *Log) Run(ctx context.Context) {
	for {
		if err := l.prune(); err != nil {
			l.log.Warn("delete old audit entries", "err", err)
		}
		select {
		case <-ctx.Done():
			l.Close()
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// Filter chooses entries. Empty fields match everything; text fields match
// case-insensitively, and Text anywhere in an entry.
type Filter struct {
	Actor  string
	Action string // a prefix, e.g. "servers." or "auth.login"
	Target string
	Server string // exactly
	Result string // exactly
	Text   string
	Since  time.Time
	// Before is the paging cursor: only entries before it.
	Before time.Time
	Limit  int
}

func contains(s, sub string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(sub)) }

func (f Filter) match(e Entry) bool {
	switch {
	case f.Actor != "" && !contains(e.Actor, f.Actor):
		return false
	case f.Action != "" && !strings.HasPrefix(strings.ToLower(e.Action), strings.ToLower(f.Action)):
		return false
	case f.Target != "" && !contains(e.Target, f.Target):
		return false
	case f.Server != "" && e.Server != f.Server:
		return false
	case f.Result != "" && e.Result != f.Result:
		return false
	case !f.Since.IsZero() && e.Time.Before(f.Since):
		return false
	case !f.Before.IsZero() && !e.Time.Before(f.Before):
		return false
	}
	if f.Text != "" {
		hay := strings.Join([]string{e.Actor, e.ActorKind, e.IP, e.Action, e.Target, e.Server, e.Result, e.Error}, " ")
		for k, v := range e.Details {
			hay += " " + k + " " + v
		}
		if !contains(hay, f.Text) {
			return false
		}
	}
	return true
}

// MaxLimit is the most entries returned at once.
const MaxLimit = 1000

// Query returns the entries that match, newest first, and the cursor for
// the next page (zero when there are no more).
func (l *Log) Query(f Filter) ([]Entry, time.Time, error) {
	if f.Limit <= 0 || f.Limit > MaxLimit {
		f.Limit = 100
	}
	months, err := l.months()
	if err != nil {
		return nil, time.Time{}, err
	}
	out := []Entry{}
	for _, m := range months {
		// Months wholly before Since, or after Before, can be skipped.
		if !f.Since.IsZero() && m < monthOf(f.Since) {
			break
		}
		if !f.Before.IsZero() && m > monthOf(f.Before) {
			continue
		}
		entries, err := l.read(m)
		if err != nil {
			return nil, time.Time{}, err
		}
		for i := len(entries) - 1; i >= 0; i-- {
			if !f.match(entries[i]) {
				continue
			}
			if len(out) == f.Limit {
				return out, out[len(out)-1].Time, nil
			}
			out = append(out, entries[i])
		}
	}
	return out, time.Time{}, nil
}

// read reads a month's entries, oldest first. A line that can't be read
// (say, cut short by a crash) is skipped.
func (l *Log) read(month string) ([]Entry, error) {
	l.mu.Lock()
	data, err := os.ReadFile(l.path(month))
	l.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Action != "" {
			out = append(out, e)
		}
	}
	// Entries are appended as they happen, but sort anyway: clocks move.
	slices.SortStableFunc(out, func(a, b Entry) int { return a.Time.Compare(b.Time) })
	return out, nil
}

// Files returns the log's files, for the configuration backup.
func (l *Log) Files() ([]string, error) {
	months, err := l.months()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range months {
		out = append(out, l.path(m))
	}
	return out, nil
}

// --- Details collected while handling a request ---

type detailsKey struct{}

// WithDetails returns a context that collects details for the request's
// entry; see Add.
func WithDetails(ctx context.Context) (context.Context, map[string]string) {
	d := map[string]string{}
	return context.WithValue(ctx, detailsKey{}, d), d
}

// Add adds a detail to the entry of the request ctx belongs to: names of
// what changed, a path, a console command… never a secret's value.
func Add(ctx context.Context, key, value string) {
	if d, ok := ctx.Value(detailsKey{}).(map[string]string); ok && value != "" {
		d[key] = value
	}
}

// serverKey is the detail SetServer uses.
const serverKey = "\x00server"

// SetServer names the server a request is about, for requests whose path
// doesn't (installing one, say).
func SetServer(ctx context.Context, name string) { Add(ctx, serverKey, name) }

// TakeServer removes and returns the server SetServer named.
func TakeServer(details map[string]string) string {
	s := details[serverKey]
	delete(details, serverKey)
	return s
}

// failKey is the detail Fail uses.
const failKey = "\x00failed"

// Fail records that a request failed after its response had started (a
// streamed install or update), so its entry isn't recorded as OK.
func Fail(ctx context.Context, err error) {
	if err != nil {
		Add(ctx, failKey, err.Error())
	}
}

// TakeFailure removes and returns the error Fail recorded.
func TakeFailure(details map[string]string) string {
	s := details[failKey]
	delete(details, failKey)
	return s
}

// Addf is Add with formatting.
func Addf(ctx context.Context, key, format string, args ...any) {
	Add(ctx, key, fmt.Sprintf(format, args...))
}
