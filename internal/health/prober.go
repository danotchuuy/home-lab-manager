package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// ProberServers is what the prober needs from the server manager.
type ProberServers interface {
	Servers
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	RunCheck(ctx context.Context, id string, hc docker.Healthcheck) (int, string, error)
}

// ProberOptions configures a Prober.
type ProberOptions struct {
	Store   *Store
	Servers ProberServers
	// HostAddresses are a server's Docker host's addresses, for the ports
	// it publishes.
	HostAddresses func(id string) []string
	// Busy says whether a server is being backed up or restored.
	Busy     func(name string) bool
	Notifier notify.Notifier
	Audit    audit.Recorder
	Log      *slog.Logger
	// For tests.
	Now  func() time.Time
	Env  *ProbeEnv
	Tick time.Duration
}

// Prober runs servers' Kubernetes-style probes.
type Prober struct {
	opts    ProberOptions
	mu      sync.Mutex
	targets map[string]*target // by server name
	wg      sync.WaitGroup
}

// target is one running server's probes.
type target struct {
	id      string
	config  string    // the probes' settings, as JSON: a change starts over
	started string    // the container's StartedAt: a restart starts over
	at      time.Time // when it started
	known   bool      // started and at are filled in
	probes  map[string]*probeState
	// passed says the startup probe succeeded (or there's none): the
	// others run from then on.
	passed     bool
	ready      bool
	readySince time.Time // in this readiness since
	restarting bool
	looking    bool // for its new start, after a restart
}

type probeState struct {
	probe     Probe
	next      time.Time
	running   bool
	result    string // "", ResultSuccess or ResultFailure
	successes int    // in a row
	failures  int    // in a row
	message   string
	checked   time.Time
}

// Probe results.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
)

// NewProber returns a prober.
func NewProber(o ProberOptions) *Prober {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Tick == 0 {
		o.Tick = time.Second
	}
	return &Prober{opts: o, targets: map[string]*target{}}
}

// Run runs probes until ctx ends.
func (p *Prober) Run(ctx context.Context) {
	t := time.NewTicker(p.opts.Tick)
	defer t.Stop()
	defer p.wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick(ctx)
		}
	}
}

// Tick starts the probes that are due.
func (p *Prober) Tick(ctx context.Context) {
	all, err := p.opts.Store.All()
	if err != nil {
		p.opts.Log.Warn("probes: read settings", "err", err)
		return
	}
	probed := map[string]string{}
	for name, set := range all {
		if !set.Probes.Empty() {
			b, _ := json.Marshal(set.Probes)
			probed[name] = string(b)
		}
	}
	if len(probed) == 0 {
		p.mu.Lock()
		clear(p.targets)
		p.mu.Unlock()
		return
	}
	list, err := p.opts.Servers.List(ctx)
	if err != nil {
		return
	}
	now := p.opts.Now()
	running := map[string]bool{}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, srv := range list {
		cfg, ok := probed[srv.Name]
		if !ok || srv.State != "running" {
			continue
		}
		running[srv.Name] = true
		t := p.targets[srv.Name]
		if t == nil || t.id != srv.ID || t.config != cfg {
			t = &target{id: srv.ID, config: cfg, probes: map[string]*probeState{}}
			all[srv.Name].Probes.each(func(name string, pr *Probe) {
				t.probes[name] = &probeState{probe: *pr, next: now}
			})
			t.passed = t.probes[ProbeStartup] == nil
			t.readySince = now
			p.targets[srv.Name] = t
		}
		if t.restarting {
			if !t.looking {
				t.looking = true
				p.wg.Add(1)
				go func() {
					defer p.wg.Done()
					p.watchRestart(ctx, srv.Name, t)
				}()
			}
			continue
		}
		for kind, ps := range t.probes {
			if ps.running || now.Before(ps.next) || !t.due(kind) {
				continue
			}
			ps.running = true
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.probe(ctx, srv.Name, t, kind, ps)
			}()
		}
	}
	for name := range p.targets {
		if !running[name] {
			delete(p.targets, name) // stopped, or no probes now
		}
	}
}

// due says whether a probe runs at this stage: until the startup probe
// succeeds only it does, and from then on only the others.
func (t *target) due(kind string) bool {
	if kind == ProbeStartup {
		return !t.passed
	}
	return t.passed
}

// probe runs one probe once and acts on what it found.
func (p *Prober) probe(ctx context.Context, name string, t *target, kind string, ps *probeState) {
	info, err := p.opts.Servers.Inspect(ctx, name)
	done := func() {
		p.mu.Lock()
		ps.running = false
		p.mu.Unlock()
	}
	if err != nil || !info.State.Running {
		done()
		return
	}
	p.mu.Lock()
	if p.targets[name] != t {
		p.mu.Unlock()
		return // started over meanwhile
	}
	t.look(info, p.opts.Now())
	if p.opts.Now().Before(ps.next) || !t.due(kind) || t.restarting {
		ps.running = false
		p.mu.Unlock()
		return
	}
	pr := ps.probe
	p.mu.Unlock()

	env := ProbeEnv{Exec: p.opts.Servers.RunCheck}
	if p.opts.Env != nil {
		env = *p.opts.Env
	}
	if p.opts.HostAddresses != nil {
		env.HostAddresses = p.opts.HostAddresses(info.ID)
	}
	timeout := time.Duration(pr.TimeoutSeconds) * time.Second
	pctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second) // a command's own limit is the timeout
	ok, msg := pr.Run(pctx, info, env)
	cancel()
	if ctx.Err() != nil {
		done()
		return
	}

	now := p.opts.Now()
	p.mu.Lock()
	if p.targets[name] != t || t.started != info.State.StartedAt {
		p.mu.Unlock()
		return
	}
	ps.running = false
	ps.checked, ps.message = now, msg
	ps.next = now.Add(time.Duration(pr.PeriodSeconds) * time.Second)
	if ok {
		ps.result, ps.successes, ps.failures = ResultSuccess, ps.successes+1, 0
	} else {
		ps.result, ps.successes, ps.failures = ResultFailure, 0, ps.failures+1
	}
	var restart, notReady string
	switch kind {
	case ProbeStartup:
		if ok {
			t.passed = true
			for k, s := range t.probes {
				if k != ProbeStartup {
					s.next = now // the others start now
				}
			}
			if t.probes[ProbeReadiness] == nil {
				t.setReady(true, now)
			}
		} else if ps.failures >= pr.FailureThreshold {
			restart = fmt.Sprintf("its startup probe failed %d times (%s)", ps.failures, msg)
		}
	case ProbeLiveness:
		if !ok && ps.failures >= pr.FailureThreshold {
			restart = fmt.Sprintf("its liveness probe failed %d times in a row (%s)", ps.failures, msg)
		}
	case ProbeReadiness:
		switch {
		case ok && ps.successes >= pr.SuccessThreshold && !t.ready:
			t.setReady(true, now)
		case !ok && ps.failures >= pr.FailureThreshold && t.ready:
			t.setReady(false, now)
			notReady = msg
		}
	}
	if restart != "" {
		if p.opts.Servers.JobRunning(name) || (p.opts.Busy != nil && p.opts.Busy(name)) {
			restart = "" // a job or backup has it: once that's done
		} else {
			t.restarting = true
		}
	}
	p.mu.Unlock()

	if notReady != "" {
		p.opts.Log.Info("server not ready", "server", name, "probe", msg)
		if p.opts.Notifier != nil {
			p.opts.Notifier.Notify(notify.Event{
				Kind: notify.KindServerUnhealthy, Title: name + " isn't ready",
				Message: fmt.Sprintf("%s's readiness probe failed %d times in a row: %s", name, ps.failures, msg),
				Server:  name, Link: "/servers/" + name + "?tab=settings", Time: now,
				Key: notify.DedupeKey(notify.KindServerUnhealthy, name) + "/ready",
			})
		}
	}
	if restart != "" {
		failed := restartServer(ctx, p.opts.Servers, p.opts.Audit, p.opts.Notifier, p.opts.Log, now, name, restart)
		if failed {
			p.mu.Lock()
			t.restarting = false
			p.mu.Unlock()
		}
	}
}

// look notes the container's start: a new one (or the first look) starts
// the probes over, each after its initial delay counted from the start.
func (t *target) look(info docker.ContainerJSON, now time.Time) {
	if t.known && t.started == info.State.StartedAt {
		return
	}
	at, _ := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	t.known, t.started, t.at = true, info.State.StartedAt, at
	t.passed = t.probes[ProbeStartup] == nil
	t.restarting = false
	t.setReady(t.passed && t.probes[ProbeReadiness] == nil, now) // with nothing to wait for, ready, as in Kubernetes
	for _, s := range t.probes {
		s.result, s.successes, s.failures, s.message, s.checked = "", 0, 0, "", time.Time{}
		s.next = at.Add(time.Duration(s.probe.InitialDelaySeconds) * time.Second)
	}
}

// watchRestart looks for the new start of a server hlm restarted.
func (p *Prober) watchRestart(ctx context.Context, name string, t *target) {
	info, err := p.opts.Servers.Inspect(ctx, name)
	p.mu.Lock()
	defer p.mu.Unlock()
	t.looking = false
	if err == nil && info.State.Running && p.targets[name] == t {
		t.look(info, p.opts.Now())
	}
}

func (t *target) setReady(ready bool, now time.Time) {
	if t.ready != ready {
		t.ready, t.readySince = ready, now
	}
}

// ProbeStatus is how a server's probes are doing.
type ProbeStatus struct {
	// Started says the startup probe succeeded, or there's none.
	Started bool `json:"started"`
	// Ready is the readiness probe's verdict (with none, ready once
	// started).
	Ready      bool                   `json:"ready"`
	ReadySince time.Time              `json:"readySince,omitzero"`
	Restarting bool                   `json:"restarting,omitempty"`
	Probes     map[string]ProbeResult `json:"probes"`
}

// ProbeResult is one probe's latest result.
type ProbeResult struct {
	Result    string    `json:"result,omitempty"` // success, failure, or empty before it has run
	Successes int       `json:"successes"`        // in a row
	Failures  int       `json:"failures"`         // in a row
	Message   string    `json:"message,omitempty"`
	Checked   time.Time `json:"checked,omitzero"`
	Next      time.Time `json:"next,omitzero"`
	Waiting   bool      `json:"waiting,omitempty"` // for the startup probe
}

// Status is a running server's probes' state, if it has probes.
func (p *Prober) Status(name string) (ProbeStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.targets[name]
	if t == nil {
		return ProbeStatus{}, false
	}
	st := ProbeStatus{Started: t.passed, Ready: t.ready, ReadySince: t.readySince, Restarting: t.restarting,
		Probes: map[string]ProbeResult{}}
	for k, s := range t.probes {
		st.Probes[k] = ProbeResult{Result: s.result, Successes: s.successes, Failures: s.failures, Message: s.message,
			Checked: s.checked, Next: s.next, Waiting: !t.due(k) && k != ProbeStartup}
	}
	return st, true
}

// Statuses are every probed server's states, by name.
func (p *Prober) Statuses() map[string]ProbeStatus {
	p.mu.Lock()
	names := make([]string, 0, len(p.targets))
	for n := range p.targets {
		names = append(names, n)
	}
	p.mu.Unlock()
	out := map[string]ProbeStatus{}
	for _, n := range names {
		if st, ok := p.Status(n); ok {
			out[n] = st
		}
	}
	return out
}

// Test runs a probe once against a server, as the prober would.
func (p *Prober) Test(ctx context.Context, name string, pr Probe) (bool, string, error) {
	pr, err := pr.validate(ProbeReadiness)
	if err != nil {
		return false, "", err
	}
	info, err := p.opts.Servers.Inspect(ctx, name)
	if err != nil {
		return false, "", err
	}
	if !info.State.Running {
		return false, "", fmt.Errorf("the server isn't running")
	}
	env := ProbeEnv{Exec: p.opts.Servers.RunCheck}
	if p.opts.Env != nil {
		env = *p.opts.Env
	}
	if p.opts.HostAddresses != nil {
		env.HostAddresses = p.opts.HostAddresses(info.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(pr.TimeoutSeconds)*time.Second+10*time.Second)
	defer cancel()
	ok, msg := pr.Run(ctx, info, env)
	return ok, msg, nil
}
