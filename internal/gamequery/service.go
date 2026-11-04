package gamequery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/reach"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Statuses of a server, as its game sees it.
const (
	StatusOnline        = "online"         // answering its status query
	StatusStarting      = "starting"       // running, not answering yet
	StatusNotResponding = "not_responding" // answered, then stopped; or never did within startGrace
	StatusUnknown       = "unknown"        // no way to ask it was found
	StatusOff           = "off"            // querying is turned off
	StatusStopped       = "stopped"        // the container isn't running
)

// Timings.
const (
	pollOnline   = 30 * time.Second
	pollStarting = 15 * time.Second
	pollConsole  = time.Minute // player lists through the console
	redetect     = 2 * time.Minute
	startGrace   = 30 * time.Minute // how long a server may take to start answering
	failsAllowed = 3                // failed queries before an online server counts as not responding
	historyStep  = 5 * time.Minute
	historyLen   = 24 * 12 // a day of 5-minute steps
	parallel     = 8
)

// Settings choose how a server is queried; the zero value finds a way.
type Settings struct {
	Protocol string `json:"protocol"`
	// Port is the container port to query; 0 uses the usual one.
	Port int `json:"port,omitempty"`
}

// Point is the most players seen in a 5-minute step.
type Point struct {
	T       time.Time `json:"t"`
	Players int       `json:"players"`
}

// Status is a server's state as its game reports it.
type Status struct {
	Status   string   `json:"status"`
	Settings Settings `json:"settings"`
	// Method is how it's being queried: the chosen or found protocol and
	// port.
	Method  *Settings `json:"method,omitempty"`
	Result  *Result   `json:"result,omitempty"` // the last answer
	Error   string    `json:"error,omitempty"`  // why the last query failed
	Checked time.Time `json:"checked,omitzero"`
	Since   time.Time `json:"since,omitzero"` // in this status since
	History []Point   `json:"history,omitempty"`
}

// Summary is the part of a status the server list shows.
type Summary struct {
	Status     string `json:"status"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"maxPlayers,omitempty"`
	HasPlayers bool   `json:"hasPlayers"`
}

// Servers is what the service needs from the server manager.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	Config(ctx context.Context, id string) (*catalog.App, deploy.Request, error)
}

// Console reads player lists through servers' consoles.
type Console interface {
	Replies(ctx context.Context, id string) (game string, ok bool)
	Query(ctx context.Context, id, command string) (string, error)
}

// Options configures a Service.
type Options struct {
	Secrets       *secrets.Store
	Servers       Servers
	Console       Console
	Notifier      notify.Notifier
	HostAddresses func() []string
	Log           *slog.Logger
	// Probes and Now are for tests.
	Probes map[string]func(ctx context.Context, addr string) (Result, error)
	Now    func() time.Time
}

// server is what's known about one server.
type server struct {
	id       string
	started  string // the container's StartedAt; a new one starts over
	status   string
	since    time.Time
	method   *Settings // found or chosen
	last     *Settings // the method that last worked, tried first again
	detected time.Time // when detection last ran (when not chosen)
	result   *Result
	err      string
	checked  time.Time
	next     time.Time
	fails    int
	answered bool // since this start
	busy     bool // a query is in flight
	history  []Point
	settings Settings
}

// Service polls servers' status queries.
type Service struct {
	opts   Options
	boot   time.Time
	saveMu sync.Mutex

	mu      sync.Mutex
	servers map[string]*server // by name
}

const secretName = "gamequery"

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HostAddresses == nil {
		o.HostAddresses = func() []string { return []string{"127.0.0.1"} }
	}
	if o.Probes == nil {
		o.Probes = map[string]func(context.Context, string) (Result, error){
			ProtoA2S: A2S, ProtoMinecraft: Minecraft, ProtoBedrock: Bedrock, ProtoTCP: TCP,
		}
	}
	return &Service{opts: o, boot: o.Now(), servers: map[string]*server{}}
}

func (s *Service) allSettings() (map[string]Settings, error) {
	m := map[string]Settings{}
	_, err := s.opts.Secrets.Get(secretName, &m)
	return m, err
}

var protocols = []string{ProtoAuto, ProtoA2S, ProtoMinecraft, ProtoBedrock, ProtoRCON, ProtoTCP, ProtoOff}

func (set Settings) check() (Settings, error) {
	if !slices.Contains(protocols, set.Protocol) {
		return set, fmt.Errorf("unknown query protocol %q", set.Protocol)
	}
	switch set.Protocol {
	case ProtoAuto, ProtoRCON, ProtoOff:
		set.Port = 0
	default:
		if set.Port < 1 || set.Port > 65535 {
			return set, errors.New("enter the port to query (in the container)")
		}
	}
	return set, nil
}

// SetSettings chooses how a server is queried.
func (s *Service) SetSettings(name string, set Settings) (Settings, error) {
	set, err := set.check()
	if err != nil {
		return set, err
	}
	s.saveMu.Lock()
	all, err := s.allSettings()
	if err == nil {
		if set == (Settings{}) {
			delete(all, name)
		} else {
			all[name] = set
		}
		err = s.opts.Secrets.Set(secretName, all)
	}
	s.saveMu.Unlock()
	if err != nil {
		return set, err
	}
	// Start over with the new settings, straight away.
	s.mu.Lock()
	if sv := s.servers[name]; sv != nil {
		sv.method, sv.detected, sv.next, sv.fails = nil, time.Time{}, time.Time{}, 0
		sv.settings = set
	}
	s.mu.Unlock()
	return set, nil
}

// ForgetServer removes a deleted server's settings and state.
func (s *Service) ForgetServer(name string) error {
	s.mu.Lock()
	delete(s.servers, name)
	s.mu.Unlock()
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all, err := s.allSettings()
	if err != nil {
		return err
	}
	if _, ok := all[name]; !ok {
		return nil
	}
	delete(all, name)
	return s.opts.Secrets.Set(secretName, all)
}

// Status returns a server's status. Without names, player names are left
// out.
func (s *Service) Status(name string, names bool) Status {
	all, _ := s.allSettings()
	s.mu.Lock()
	defer s.mu.Unlock()
	sv := s.servers[name]
	if sv == nil {
		return Status{Status: StatusStopped, Settings: all[name]}
	}
	st := Status{Status: sv.status, Settings: all[name], Error: sv.err, Checked: sv.checked, Since: sv.since,
		History: slices.Clone(sv.history)}
	if sv.method != nil {
		m := *sv.method
		st.Method = &m
	}
	if sv.result != nil {
		r := *sv.result
		if !names {
			r.PlayerNames = nil
		}
		st.Result = &r
	}
	return st
}

// Summaries returns each running server's summary, for those keep accepts.
func (s *Service) Summaries(keep func(string) bool) map[string]Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Summary{}
	for name, sv := range s.servers {
		if !keep(name) {
			continue
		}
		sum := Summary{Status: sv.status}
		if sv.result != nil && sv.status == StatusOnline {
			sum.Players, sum.MaxPlayers, sum.HasPlayers = sv.result.Players, sv.result.MaxPlayers, sv.result.HasPlayers
		}
		out[name] = sum
	}
	return out
}

// Players returns how many players are on a server, and whether that's
// known (the server is online and its query counts players).
func (s *Service) Players(name string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sv := s.servers[name]
	if sv == nil || sv.status != StatusOnline || sv.result == nil || !sv.result.HasPlayers {
		return 0, false
	}
	return sv.result.Players, true
}

// Run polls running servers until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		s.tick(ctx, func(name string) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				s.poll(ctx, name)
			}()
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick updates the list of running servers and starts the queries that
// are due.
func (s *Service) tick(ctx context.Context, start func(name string)) {
	list, err := s.opts.Servers.List(ctx)
	if err != nil {
		return
	}
	all, _ := s.allSettings()
	now := s.opts.Now()
	running := map[string]bool{}
	var due []string
	s.mu.Lock()
	for _, srv := range list {
		if srv.State != "running" {
			continue
		}
		running[srv.Name] = true
		sv := s.servers[srv.Name]
		if sv == nil || sv.id != srv.ID {
			sv = &server{id: srv.ID, status: StatusStarting, since: now}
			s.servers[srv.Name] = sv
		}
		sv.settings = all[srv.Name]
		if sv.settings.Protocol == ProtoOff {
			sv.status, sv.result, sv.err = StatusOff, nil, ""
			continue
		}
		if !sv.busy && !now.Before(sv.next) {
			sv.busy = true
			due = append(due, srv.Name)
		}
	}
	for name := range s.servers {
		if !running[name] {
			delete(s.servers, name) // stopped: start over when it runs again
		}
	}
	s.mu.Unlock()
	for _, name := range due {
		start(name)
	}
}

// poll queries one server and updates its status.
func (s *Service) poll(ctx context.Context, name string) {
	defer func() {
		s.mu.Lock()
		if sv := s.servers[name]; sv != nil {
			sv.busy = false
		}
		s.mu.Unlock()
	}()
	info, err := s.opts.Servers.Inspect(ctx, name)
	if err != nil {
		return
	}
	s.mu.Lock()
	sv := s.servers[name]
	if sv == nil {
		s.mu.Unlock()
		return
	}
	if sv.started != info.State.StartedAt {
		// Started (again) since last time: forget what was seen before.
		sv.started, sv.answered, sv.fails, sv.result = info.State.StartedAt, false, 0, nil
		sv.status, sv.since = StatusStarting, s.opts.Now()
	}
	set, method, last, detected := sv.settings, sv.method, sv.last, sv.detected
	s.mu.Unlock()

	now := s.opts.Now()
	var res Result
	if set.Protocol != ProtoAuto {
		method = &Settings{Protocol: set.Protocol, Port: set.Port}
		res, err = s.query(ctx, info, *method)
		s.record(name, info, method, detected, res, err)
		return
	}
	// Automatic: try the way that worked before, and look for another now
	// and then while it doesn't.
	if method == nil {
		method = last
	}
	if method != nil {
		res, err = s.query(ctx, info, *method)
	}
	if (method == nil || err != nil) && (detected.IsZero() || now.Sub(detected) >= redetect) {
		found, fres, ferr := s.detect(ctx, info)
		detected = now
		if ferr == nil || method == nil {
			method, res, err = found, fres, ferr
		}
	} else if method == nil {
		err = errNoMethod
	}
	s.record(name, info, method, detected, res, err)
}

var errNoMethod = errors.New("no way to ask this server how it's doing was found; choose one in its query settings")

// query asks a server once, trying each address it might be at.
func (s *Service) query(ctx context.Context, info docker.ContainerJSON, m Settings) (Result, error) {
	if m.Protocol == ProtoRCON {
		return s.queryConsole(ctx, info)
	}
	probe := s.opts.Probes[m.Protocol]
	if probe == nil {
		return Result{}, fmt.Errorf("unknown protocol %q", m.Protocol)
	}
	proto := "udp"
	if m.Protocol == ProtoMinecraft || m.Protocol == ProtoTCP {
		proto = "tcp"
	}
	addrs := reach.Addresses(info, m.Port, proto, s.opts.HostAddresses())
	if len(addrs) == 0 {
		return Result{}, errors.New("hlm can't reach this server: it has no address on a network hlm can use, and the port isn't published")
	}
	var errs []string
	for _, a := range addrs {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		res, err := probe(cctx, a)
		cancel()
		if err == nil {
			return res, nil
		}
		errs = append(errs, a+": "+shortError(err))
	}
	return Result{}, errors.New(strings.Join(errs, "; "))
}

// shortError turns Go's long network errors into a few words.
func shortError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return "no answer"
	case strings.Contains(msg, "connection refused"):
		return "nothing is listening on that port"
	case strings.Contains(msg, "no route to host"):
		return "no route to host (is the server on a macvlan network hlm isn't connected to?)"
	case strings.Contains(msg, "network is unreachable"):
		return "network unreachable"
	}
	if i := strings.LastIndex(msg, ": "); i >= 0 && (strings.HasPrefix(msg, "dial ") || strings.HasPrefix(msg, "read ")) {
		return msg[i+2:]
	}
	return msg
}

func (s *Service) queryConsole(ctx context.Context, info docker.ContainerJSON) (Result, error) {
	if s.opts.Console == nil {
		return Result{}, errors.New("the console isn't available")
	}
	game := consoleGame(info.Config.Image)
	cmd := rconCommands[game]
	if cmd == "" {
		return Result{}, errors.New("hlm doesn't know this game's player list command")
	}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := s.opts.Console.Query(cctx, strings.TrimPrefix(info.Name, "/"), cmd)
	if err != nil {
		return Result{}, err
	}
	res, err := parsePlayerList(game, out)
	if err != nil {
		return Result{}, err
	}
	res.Protocol, res.Latency = ProtoRCON, time.Since(start)
	return res, nil
}

// detect looks for a way to query a server, trying each candidate.
func (s *Service) detect(ctx context.Context, info docker.ContainerJSON) (*Settings, Result, error) {
	name := strings.TrimPrefix(info.Name, "/")
	app, _, _ := s.opts.Servers.Config(ctx, name)
	replies := false
	if s.opts.Console != nil {
		_, replies = s.opts.Console.Replies(ctx, name)
	}
	cands := candidates(info, app, replies)
	if len(cands) == 0 {
		return nil, Result{}, errNoMethod
	}
	var lastErr error
	for _, c := range cands {
		if ctx.Err() != nil {
			return nil, Result{}, ctx.Err()
		}
		m := Settings{Protocol: c.Protocol, Port: c.Port}
		res, err := s.query(ctx, info, m)
		if err == nil {
			return &m, res, nil
		}
		lastErr = err
	}
	return nil, Result{}, fmt.Errorf("%w (tried %d ways; last: %v)", errNoMethod, len(cands), lastErr)
}

// record updates a server's status after a query, and reports changes.
func (s *Service) record(name string, info docker.ContainerJSON, method *Settings, detected time.Time, res Result, err error) {
	now := s.opts.Now()
	s.mu.Lock()
	sv := s.servers[name]
	if sv == nil || sv.started != info.State.StartedAt {
		s.mu.Unlock()
		return
	}
	sv.checked, sv.detected = now, detected
	setStatus := func(st string) {
		if sv.status != st {
			sv.status, sv.since = st, now
		}
	}
	var event *notify.Event
	if err == nil {
		sv.method, sv.last, sv.err, sv.fails = method, method, "", 0
		r := res
		sv.result = &r
		wasDown := sv.status != StatusOnline
		first := !sv.answered
		sv.answered = true
		setStatus(StatusOnline)
		sv.addPoint(now, res)
		if wasDown && s.opts.Notifier != nil {
			s.opts.Notifier.Clear(notify.DedupeKey(notify.KindNotResponding, name))
		}
		// Only for starts hlm saw, not servers already up when hlm started.
		if first && startedAt(info).After(s.boot) {
			msg := name + " answered its status query"
			if res.HasPlayers {
				msg += fmt.Sprintf(": %d of %d players", res.Players, res.MaxPlayers)
			}
			event = &notify.Event{Kind: notify.KindServerOnline, Title: name + " is ready", Message: msg + ".",
				Server: name, Link: "/servers/" + name}
		}
	} else {
		sv.err = err.Error()
		sv.fails++
		// A way that was found, not chosen, may have stopped working (the
		// port changed, say): look for one again.
		if sv.settings.Protocol == ProtoAuto && sv.fails >= failsAllowed {
			sv.method = nil
		}
		var next string
		switch {
		case sv.answered && sv.fails >= failsAllowed:
			next = StatusNotResponding
		case sv.answered:
			next = sv.status // a failure or two is tolerated
		case now.Sub(startedAt(info)) <= startGrace:
			next = StatusStarting
		case method == nil && sv.settings.Protocol == ProtoAuto:
			next = StatusUnknown // no query found: maybe the game hasn't one hlm knows
		default:
			next = StatusNotResponding
		}
		if next == StatusNotResponding && sv.status != StatusNotResponding {
			title, msg := name+" isn't responding", name+" is running but stopped answering its status query: "+sv.err
			if !sv.answered {
				title = name + " hasn't come up"
				msg = fmt.Sprintf("%s has been running for over %d minutes but still doesn't answer its status query: %s",
					name, int(startGrace.Minutes()), sv.err)
			}
			event = &notify.Event{Kind: notify.KindNotResponding, Title: title, Message: msg, Server: name, Link: "/servers/" + name}
		}
		setStatus(next)
	}
	// When to ask next.
	wait := pollOnline
	switch {
	case sv.method == nil && sv.settings.Protocol == ProtoAuto:
		wait = pollStarting
		if !sv.answered && now.Sub(startedAt(info)) > startGrace {
			wait = redetect
		}
	case (sv.method != nil && sv.method.Protocol == ProtoRCON) || sv.settings.Protocol == ProtoRCON:
		wait = pollConsole
	case sv.status != StatusOnline || sv.fails > 0:
		wait = pollStarting // confirm a failure, or catch it coming up, sooner
	}
	sv.next = now.Add(wait)
	s.mu.Unlock()
	if event != nil && s.opts.Notifier != nil {
		s.opts.Notifier.Notify(*event)
	}
}

func startedAt(info docker.ContainerJSON) time.Time {
	t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// addPoint records the player count in the 5-minute history.
func (sv *server) addPoint(now time.Time, res Result) {
	if !res.HasPlayers {
		return
	}
	step := now.Truncate(historyStep)
	if n := len(sv.history); n > 0 && sv.history[n-1].T.Equal(step) {
		sv.history[n-1].Players = max(sv.history[n-1].Players, res.Players)
		return
	}
	sv.history = append(sv.history, Point{T: step, Players: res.Players})
	if over := len(sv.history) - historyLen; over > 0 {
		sv.history = slices.Delete(sv.history, 0, over)
	}
}

// Test queries a server with the given settings, without saving them;
// automatic settings run detection.
func (s *Service) Test(ctx context.Context, name string, set Settings) (Settings, Result, error) {
	set, err := set.check()
	if err != nil {
		return set, Result{}, err
	}
	info, err := s.opts.Servers.Inspect(ctx, name)
	if err != nil {
		return set, Result{}, err
	}
	if !info.State.Running {
		return set, Result{}, errors.New("start the server first")
	}
	switch set.Protocol {
	case ProtoOff:
		return set, Result{}, errors.New("querying is off")
	case ProtoAuto:
		m, res, err := s.detect(ctx, info)
		if m != nil {
			set = *m
		}
		return set, res, err
	}
	res, err := s.query(ctx, info, set)
	return set, res, err
}
