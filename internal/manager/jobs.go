package manager

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// keepFinished is how long a finished job stays visible, so clients polling
// the job list see how it ended.
const keepFinished = time.Minute

// Job is an install, update or reconfigure in progress (or recently
// finished), as shown to every client.
type Job struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"` // install, update, reconfigure
	Server   string     `json:"server"`
	App      string     `json:"app,omitempty"`
	Icon     string     `json:"icon,omitempty"`
	Image    string     `json:"image,omitempty"`
	Stage    string     `json:"stage"` // last event stage: pull, stop, create, start, ...
	Message  string     `json:"message,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	Error    string     `json:"error,omitempty"`
	Pull     PullStatus `json:"pull"`
}

// PullStatus summarizes an image download.
type PullStatus struct {
	Layers     int   `json:"layers"`     // layers in the image
	Done       int   `json:"done"`       // layers downloaded (or already present)
	Downloaded int64 `json:"downloaded"` // bytes downloaded so far
	Total      int64 `json:"total"`      // bytes to download, as far as known
	// Extracting is set once every layer is downloaded and Docker is
	// unpacking them.
	Extracting bool `json:"extracting"`
}

type layer struct {
	current, total int64
	done           bool
}

type jobState struct {
	mu     sync.Mutex
	job    Job
	layers map[string]*layer
}

// jobTracker records running jobs in memory.
type jobTracker struct {
	mu   sync.Mutex
	next int
	jobs map[string]*jobState
}

func newJobTracker() *jobTracker {
	return &jobTracker{jobs: map[string]*jobState{}}
}

// start registers a job and returns an emit function that records each event
// on the job before passing it on, and a function to call when it ends. It
// fails if another job for the same server is still running.
func (t *jobTracker) start(job Job, emit func(Event)) (func(Event), func(error), error) {
	t.mu.Lock()
	for _, js := range t.jobs {
		js.mu.Lock()
		busy := js.job.Server == job.Server && js.job.Finished == nil
		kind := js.job.Kind
		js.mu.Unlock()
		if busy {
			t.mu.Unlock()
			return nil, nil, fmt.Errorf("%s of %s is already in progress", kind, job.Server)
		}
	}
	t.next++
	job.ID = strconv.Itoa(t.next)
	job.Started = time.Now()
	js := &jobState{job: job, layers: map[string]*layer{}}
	t.jobs[job.ID] = js
	t.mu.Unlock()

	record := func(e Event) {
		js.observe(e)
		emit(e)
	}
	finish := func(err error) {
		js.mu.Lock()
		now := time.Now()
		js.job.Finished = &now
		if err != nil {
			js.job.Stage, js.job.Error = "error", err.Error()
		} else {
			js.job.Stage = "done"
		}
		js.mu.Unlock()
		time.AfterFunc(keepFinished, func() {
			t.mu.Lock()
			delete(t.jobs, job.ID)
			t.mu.Unlock()
		})
	}
	return record, finish, nil
}

func (js *jobState) observe(e Event) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if e.Stage != "warning" {
		js.job.Stage = e.Stage
	}
	if e.Message != "" && e.Stage != "warning" {
		js.job.Message = e.Message
	}
	if p := e.Progress; p != nil && p.ID != "" {
		js.observePull(p)
	}
}

// observePull folds one pull progress message into the totals.
func (js *jobState) observePull(p *docker.PullProgress) {
	status := p.Status
	if strings.HasPrefix(status, "Pulling from") {
		return // the repository line, not a layer
	}
	l, ok := js.layers[p.ID]
	if !ok {
		l = &layer{}
		js.layers[p.ID] = l
	}
	switch {
	case status == "Downloading":
		if p.ProgressDetail.Total > 0 {
			l.current, l.total = p.ProgressDetail.Current, p.ProgressDetail.Total
		}
	case status == "Download complete", status == "Pull complete", status == "Already exists":
		l.done = true
		if l.total > 0 {
			l.current = l.total
		}
	}

	ps := PullStatus{Layers: len(js.layers)}
	extracting := status == "Extracting"
	for _, l := range js.layers {
		ps.Downloaded += l.current
		ps.Total += l.total
		if l.done {
			ps.Done++
		}
	}
	ps.Extracting = extracting && ps.Done == ps.Layers
	js.job.Pull = ps
}

// list returns a snapshot of all jobs, oldest first.
func (t *jobTracker) list() []Job {
	t.mu.Lock()
	states := make([]*jobState, 0, len(t.jobs))
	for _, js := range t.jobs {
		states = append(states, js)
	}
	t.mu.Unlock()

	out := make([]Job, 0, len(states))
	for _, js := range states {
		js.mu.Lock()
		out = append(out, js.job)
		js.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Busy reports why hlm shouldn't restart right now (a server is being
// installed or updated), or "" if nothing is running.
func (m *Manager) Busy() string {
	for _, j := range m.jobs.list() {
		if j.Finished == nil {
			return fmt.Sprintf("wait for the %s of %s to finish first", j.Kind, j.Server)
		}
	}
	return ""
}

// running reports whether a job for server is in progress.
func (t *jobTracker) running(server string) bool {
	for _, j := range t.list() {
		if j.Server == server && j.Finished == nil {
			return true
		}
	}
	return false
}

// JobRunning reports whether an install, update or reconfigure of the
// server called name is in progress.
func (m *Manager) JobRunning(name string) bool { return m.jobs.running(name) }

// Jobs returns running and recently finished installs and updates.
func (m *Manager) Jobs() []Job {
	return m.jobs.list()
}
