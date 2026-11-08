// Package restoretest checks that a server's backups really restore: it
// restores one into a throwaway, isolated copy of the server, starts it,
// and waits for the game to answer its status query. The copy and its data
// are removed whatever happens.
package restoretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretName = "restore-tests"
	// keepResults is how many results are kept per server.
	keepResults = 20
	// logLines is how much of a failed copy's log is kept.
	logLines = 50
	// DefaultTimeout is how long a copy gets to answer.
	DefaultTimeout = 10 * time.Minute
	maxTimeout     = 2 * time.Hour
	// unqueryable is how long a copy of a server that can't be queried
	// has to keep running to pass.
	unqueryable = time.Minute
)

// How often tests run.
const (
	EveryNever   = ""
	EveryWeek    = "week"
	EveryBackups = "backups"
)

// Settings say when a server's backups are tested.
type Settings struct {
	// Every is "", week or backups.
	Every string `json:"every"`
	// Backups is N for "after every Nth backup".
	Backups int `json:"backups,omitempty"`
	// TimeoutMinutes is how long the copy gets to answer (10 if 0).
	TimeoutMinutes int `json:"timeoutMinutes,omitempty"`
}

func (set Settings) timeout() time.Duration {
	if set.TimeoutMinutes <= 0 {
		return DefaultTimeout
	}
	return time.Duration(set.TimeoutMinutes) * time.Minute
}

// Outcomes.
const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
)

// Result is one test.
type Result struct {
	Key      string    `json:"key"`
	Trigger  string    `json:"trigger"` // schedule or manual
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Outcome  string    `json:"outcome"`
	// Seconds is how long the copy took to answer, once restored.
	Seconds int `json:"seconds,omitempty"`
	// Answer says what the copy's game said, when it passed.
	Answer string `json:"answer,omitempty"`
	// Note is something to know about a pass or skip.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
	// Log is the end of a failed copy's log.
	Log   string `json:"log,omitempty"`
	Files int    `json:"files,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

type stored struct {
	Settings map[string]Settings `json:"settings"`
	Results  map[string][]Result `json:"results"`
}

// Servers is what a test needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	CreateTestCopy(ctx context.Context, server string) (*manager.TestCopy, error)
	RestoreTestCopy(ctx context.Context, tc *manager.TestCopy, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error)
	StartTestCopy(ctx context.Context, tc *manager.TestCopy) error
	InspectTestCopy(ctx context.Context, tc *manager.TestCopy) (docker.ContainerJSON, error)
	TestCopyLogs(ctx context.Context, tc *manager.TestCopy, lines int) string
	RemoveTestCopy(ctx context.Context, tc *manager.TestCopy) error
	RemoveTestCopies(ctx context.Context, server string) error
	StartTestProbe(ctx context.Context, tc *manager.TestCopy, probeCommand string, args []string) error
	TestProbe(ctx context.Context, tc *manager.TestCopy) (output string, exitCode int, done bool, err error)
	StartJob(job manager.Job, emit func(manager.Event)) (func(manager.Event), func(error), error)
	List(ctx context.Context) ([]manager.Server, error)
}

// Backups is what a test needs from the backup service.
type Backups interface {
	Overview(ctx context.Context, id string) (backup.Overview, error)
	OpenBackup(ctx context.Context, id, key string) (io.ReadCloser, error)
	Busy(name string) bool
}

// ErrQueryOff is a server whose status query is turned off.
var ErrQueryOff = errors.New("the server's status query is off")

// Options configures the service.
type Options struct {
	Secrets *secrets.Store
	Servers Servers
	Backups Backups
	// Probe asks a copy's game whether it's up, as the server is asked,
	// for copies on hlm's own host.
	Probe func(ctx context.Context, server string, copy docker.ContainerJSON) (gamequery.Result, error)
	// Methods are the ways to ask a copy, for the probe helper that asks
	// copies on other hosts.
	Methods func(ctx context.Context, server string, copy docker.ContainerJSON) ([]gamequery.Settings, error)
	// QueryOff says the server's status query is turned off.
	QueryOff func(server string) bool
	// Memory says how much memory the host has free, and how much the
	// server uses; ok is false when either isn't known.
	Memory   func(ctx context.Context, server string) (free, uses uint64, ok bool)
	Notifier notify.Notifier
	Log      *slog.Logger
	Now      func() time.Time
	// Poll is how often a starting copy is checked (5s).
	Poll time.Duration
	// Unqueryable replaces unqueryable (tests).
	Unqueryable time.Duration
}

// Service runs restore tests.
type Service struct {
	opts Options
	mu   sync.Mutex
	// one test at a time, on the whole host: a copy can need as much
	// memory as the server.
	running sync.Mutex
}

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Poll == 0 {
		o.Poll = 5 * time.Second
	}
	if o.Unqueryable == 0 {
		o.Unqueryable = unqueryable
	}
	return &Service{opts: o}
}

func (s *Service) load() (stored, error) {
	var st stored
	_, err := s.opts.Secrets.Get(secretName, &st)
	if st.Settings == nil {
		st.Settings = map[string]Settings{}
	}
	if st.Results == nil {
		st.Results = map[string][]Result{}
	}
	return st, err
}

// Overview is a server's settings and results, newest first.
type Overview struct {
	Settings Settings `json:"settings"`
	Results  []Result `json:"results"`
	// Running is the backup being tested now, if one is.
	Running string `json:"running,omitempty"`
}

// Get returns a server's settings and results.
func (s *Service) Get(server string) (Overview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{Settings: st.Settings[server], Results: st.Results[server]}
	if ov.Results == nil {
		ov.Results = []Result{}
	}
	return ov, nil
}

// SetSettings saves when a server's backups are tested.
func (s *Service) SetSettings(server string, in Settings) (Settings, error) {
	switch in.Every {
	case EveryNever, EveryWeek:
		in.Backups = 0
	case EveryBackups:
		if in.Backups < 1 || in.Backups > 100 {
			return Settings{}, errors.New("after every 1 to 100 backups")
		}
	default:
		return Settings{}, fmt.Errorf("unknown schedule %q (week or backups)", in.Every)
	}
	if in.TimeoutMinutes < 0 || time.Duration(in.TimeoutMinutes)*time.Minute > maxTimeout {
		return Settings{}, fmt.Errorf("the time to answer is at most %d minutes", int(maxTimeout.Minutes()))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Settings{}, err
	}
	if in == (Settings{}) {
		delete(st.Settings, server)
	} else {
		st.Settings[server] = in
	}
	return in, s.opts.Secrets.Set(secretName, st)
}

// ForgetServer drops a deleted server's settings and results.
func (s *Service) ForgetServer(server string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	delete(st.Settings, server)
	delete(st.Results, server)
	return s.opts.Secrets.Set(secretName, st)
}

func (s *Service) record(server string, r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		s.opts.Log.Warn("restore test: record", "err", err)
		return
	}
	list := append([]Result{r}, st.Results[server]...)
	if len(list) > keepResults {
		list = list[:keepResults]
	}
	st.Results[server] = list
	if err := s.opts.Secrets.Set(secretName, st); err != nil {
		s.opts.Log.Warn("restore test: record", "err", err)
	}
}

// Test restores a backup (the newest if key is "") into a copy of the
// server, starts it and waits for it to answer, then removes the copy.
// A test that couldn't run (no backups, the server is busy) is an error;
// one that ran is a Result, passed, failed or skipped.
func (s *Service) Test(ctx context.Context, server, key, trigger string, emit func(manager.Event)) (Result, error) {
	if emit == nil {
		emit = func(manager.Event) {}
	}
	info, err := s.opts.Servers.Inspect(ctx, server)
	if err != nil {
		return Result{}, err
	}
	server = strings.TrimPrefix(info.Name, "/")
	if key == "" {
		ov, err := s.opts.Backups.Overview(ctx, server)
		if err != nil {
			return Result{}, err
		}
		if ov.Error != "" {
			return Result{}, errors.New(ov.Error)
		}
		if len(ov.Backups) == 0 {
			return Result{}, errors.New("the server has no backups to test")
		}
		key = ov.Backups[0].Key
	}
	if s.opts.Backups.Busy(server) {
		return Result{}, errors.New("a backup or restore of this server is running; try again when it's done")
	}
	if !s.running.TryLock() {
		return Result{}, errors.New("another restore test is running; one runs at a time")
	}
	defer s.running.Unlock()
	emit, finish, err := s.opts.Servers.StartJob(manager.Job{Kind: "restore test", Server: server, Image: info.Config.Image}, emit)
	if err != nil {
		return Result{}, err
	}
	set, _ := s.Get(server)
	res := s.run(ctx, server, key, set.Settings.timeout(), emit)
	res.Trigger = trigger
	if res.Outcome == Failed {
		finish(errors.New(res.Error))
	} else {
		finish(nil)
	}
	s.record(server, res)
	s.notify(server, res)
	return res, nil
}

func (s *Service) run(ctx context.Context, server, key string, timeout time.Duration, emit func(manager.Event)) (res Result) {
	res = Result{Key: key, Started: s.opts.Now().UTC()}
	say := func(stage, format string, a ...any) {
		emit(manager.Event{Stage: stage, Message: fmt.Sprintf(format, a...)})
	}
	defer func() { res.Finished = s.opts.Now().UTC() }()
	fail := func(format string, a ...any) Result {
		res.Outcome, res.Error = Failed, fmt.Sprintf(format, a...)
		return res
	}

	if s.opts.Memory != nil {
		if free, uses, ok := s.opts.Memory(ctx, server); ok && free < uses {
			res.Outcome = Skipped
			res.Note = fmt.Sprintf("not enough free memory: the host has %s free, and the server uses %s", mib(free), mib(uses))
			say("skipped", "Skipped: %s", res.Note)
			return res
		}
	}

	say("create", "Making an isolated copy of %s", server)
	tc, err := s.opts.Servers.CreateTestCopy(ctx, server)
	if err != nil {
		return fail("couldn't make the copy: %v", err)
	}
	defer func() {
		say("cleanup", "Removing the copy")
		if err := s.opts.Servers.RemoveTestCopy(context.WithoutCancel(ctx), tc); err != nil {
			s.opts.Log.Warn("restore test: remove the copy", "server", server, "err", err)
			if res.Note == "" {
				res.Note = "removing the copy failed (hlm tries again at the next test): " + err.Error()
			}
		}
	}()

	say("restore", "Restoring %s into the copy", key)
	rc, err := s.opts.Backups.OpenBackup(ctx, server, key)
	if err != nil {
		return fail("couldn't open the backup: %v", err)
	}
	st, err := s.opts.Servers.RestoreTestCopy(ctx, tc, rc, nil)
	rc.Close()
	res.Files, res.Bytes = st.Files, st.Bytes
	if err != nil {
		return fail("couldn't restore the backup: %v", err)
	}
	if st.Files == 0 {
		return fail("the backup has no files")
	}

	say("start", "Starting the copy")
	if err := s.opts.Servers.StartTestCopy(ctx, tc); err != nil {
		return fail("the copy didn't start: %v", err)
	}
	began := time.Now()
	queryOff := s.opts.QueryOff != nil && s.opts.QueryOff(server)
	if queryOff {
		say("wait", "Waiting %s to see that it keeps running (its status query is off)", s.opts.Unqueryable)
	} else {
		say("wait", "Waiting for the copy's game to answer (up to %s)", timeout)
	}
	logs := func() string {
		return notify.Redact(s.opts.Servers.TestCopyLogs(context.WithoutCancel(ctx), tc, logLines))
	}
	var lastErr error
	// On another host, hlm can't reach the copy's network: a helper next
	// to it asks, and says when the game answered or it gave up.
	probe := s.opts.Probe
	if tc.Remote() && !queryOff {
		info, err := s.opts.Servers.InspectTestCopy(ctx, tc)
		if err != nil {
			return fail("couldn't look at the copy: %v", err)
		}
		if s.opts.Methods == nil {
			return fail("hlm can't ask copies on other hosts")
		}
		methods, err := s.opts.Methods(ctx, server, info)
		if err != nil {
			return fail("hlm doesn't know how to ask the copy whether it's up: %v", err)
		}
		args := []string{"--host", tc.Name, "--methods", gamequery.FormatMethods(methods),
			"--timeout", timeout.String(), "--interval", max(s.opts.Poll, time.Second).String()}
		say("wait", "Asking from a probe next to the copy, on its host")
		if err := s.opts.Servers.StartTestProbe(ctx, tc, gamequery.ProbeCommand, args); err != nil {
			return fail("couldn't start the probe next to the copy: %v", err)
		}
		probe = func(ctx context.Context, _ string, _ docker.ContainerJSON) (gamequery.Result, error) {
			out, code, done, err := s.opts.Servers.TestProbe(ctx, tc)
			if err != nil {
				return gamequery.Result{}, err
			}
			if !done {
				return gamequery.Result{}, errNotYet
			}
			var po gamequery.ProbeOutput
			if jerr := json.Unmarshal([]byte(lastLine(out)), &po); jerr != nil {
				return gamequery.Result{}, &probeGaveUp{fmt.Sprintf("the probe ended (exit code %d) without an answer: %s", code, strings.TrimSpace(out))}
			}
			if code == 0 && po.Result != nil {
				return *po.Result, nil
			}
			return gamequery.Result{}, &probeGaveUp{po.Error}
		}
	}
	for {
		info, err := s.opts.Servers.InspectTestCopy(ctx, tc)
		if err != nil {
			return fail("couldn't look at the copy: %v", err)
		}
		if !info.State.Running {
			res.Log = logs()
			return fail("the copy stopped (exit code %d) after %s", info.State.ExitCode, round(time.Since(began)))
		}
		switch {
		case queryOff:
			if time.Since(began) >= s.opts.Unqueryable {
				res.Outcome, res.Seconds = Passed, int(time.Since(began).Seconds())
				res.Note = "its status query is off, so hlm only checked that the copy kept running"
				say("done", "Passed: the copy kept running")
				return res
			}
		default:
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			ans, err := probe(pctx, server, info)
			cancel()
			var gaveUp *probeGaveUp
			if errors.As(err, &gaveUp) {
				res.Log = logs()
				return fail("the copy didn't answer its status query within %s (last: %s)", timeout, gaveUp.msg)
			}
			if err == nil {
				res.Outcome, res.Seconds = Passed, int(time.Since(began).Seconds())
				res.Answer = answer(ans)
				say("done", "Passed: the copy answered after %s (%s)", round(time.Since(began)), res.Answer)
				return res
			}
			if !errors.Is(err, errNotYet) {
				lastErr = err
			}
		}
		if time.Since(began) >= timeout {
			res.Log = logs()
			return fail("the copy didn't answer its status query within %s (last: %v)", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return fail("stopped: %v", ctx.Err())
		case <-time.After(s.opts.Poll):
		}
	}
}

// answer describes what a game said.
func answer(r gamequery.Result) string {
	var parts []string
	if r.Version != "" {
		parts = append(parts, r.Version)
	}
	if r.Map != "" {
		parts = append(parts, r.Map)
	}
	if r.HasPlayers {
		if r.MaxPlayers > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d players", r.Players, r.MaxPlayers))
		} else {
			parts = append(parts, fmt.Sprintf("%d players", r.Players))
		}
	}
	if len(parts) == 0 {
		return "answered on " + r.Protocol
	}
	return strings.Join(parts, ", ")
}

func round(d time.Duration) time.Duration { return d.Round(time.Second) }

func mib(n uint64) string { return fmt.Sprintf("%d MiB", n>>20) }

func (s *Service) notify(server string, r Result) {
	if s.opts.Notifier == nil || r.Outcome != Failed {
		return
	}
	s.opts.Notifier.Notify(notify.Event{Kind: notify.KindRestoreTestFailed, Severity: notify.Error,
		Title:   "A backup of " + server + " didn't pass its restore test",
		Message: r.Error, Server: server, Link: "/servers/" + server + "?tab=backups",
		Fields: map[string]string{"Backup": r.Key}})
}

// due says whether a server's next test is due: the newest backup isn't
// tested yet, and the schedule says it's time.
func due(set Settings, results []Result, backups []string, now time.Time) bool {
	if set.Every == EveryNever || len(backups) == 0 {
		return false
	}
	var last *Result
	for i := range results {
		if results[i].Outcome != Skipped {
			last = &results[i]
			break
		}
	}
	if last != nil && last.Key == backups[0] {
		return false // the newest is tested
	}
	// A skipped test is tried again after a day, not at once.
	if len(results) > 0 && results[0].Outcome == Skipped && now.Sub(results[0].Finished) < 24*time.Hour {
		return false
	}
	switch set.Every {
	case EveryWeek:
		return last == nil || now.Sub(last.Finished) >= 7*24*time.Hour
	case EveryBackups:
		if last == nil {
			return true
		}
		newer := 0
		for _, k := range backups {
			if k == last.Key {
				break
			}
			newer++
		}
		return newer >= set.Backups
	}
	return false
}

// Run removes copies left by an earlier hlm, then runs the tests that come
// due, one at a time, until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	if err := s.opts.Servers.RemoveTestCopies(ctx, ""); err != nil {
		s.opts.Log.Warn("restore test: remove leftover copies", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.runDue(ctx)
	}
}

func (s *Service) runDue(ctx context.Context) {
	st, err := s.load()
	if err != nil {
		return
	}
	for server, set := range st.Settings {
		if ctx.Err() != nil {
			return
		}
		if set.Every == EveryNever || s.opts.Backups.Busy(server) {
			continue
		}
		ov, err := s.opts.Backups.Overview(ctx, server)
		if err != nil || ov.Error != "" {
			continue
		}
		keys := make([]string, len(ov.Backups))
		for i, b := range ov.Backups {
			keys[i] = b.Key
		}
		if !due(set, st.Results[server], keys, s.opts.Now()) {
			continue
		}
		s.opts.Log.Info("restore test due", "server", server, "backup", keys[0])
		if _, err := s.Test(ctx, server, keys[0], "schedule", nil); err != nil {
			s.opts.Log.Info("restore test didn't run", "server", server, "err", err)
		}
	}
}

// errNotYet is a probe helper still asking.
var errNotYet = errors.New("still asking")

// probeGaveUp is a probe helper that stopped without an answer.
type probeGaveUp struct{ msg string }

func (e *probeGaveUp) Error() string { return e.msg }

// lastLine is the last non-empty line of a log.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// RenameServer moves a server's restore test settings and results to its new name, when it moves to
// another Docker host (its name gains or changes its @host).
func (s *Service) RenameServer(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	moved := false
	if v, ok := st.Settings[from]; ok {
		st.Settings[to] = v
		delete(st.Settings, from)
		moved = true
	}
	if v, ok := st.Results[from]; ok {
		st.Results[to] = v
		delete(st.Results, from)
		moved = true
	}
	if !moved {
		return nil
	}
	return s.opts.Secrets.Set(secretName, st)
}
