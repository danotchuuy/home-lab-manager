package manager

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func pull(id, status string, current, total int64) Event {
	p := &docker.PullProgress{ID: id, Status: status}
	p.ProgressDetail.Current, p.ProgressDetail.Total = current, total
	return Event{Stage: "pull", Progress: p}
}

func TestJobPullProgress(t *testing.T) {
	tr := newJobTracker()
	var passed int
	emit, finish, err := tr.start(Job{Kind: "install", Server: "valheim"}, func(Event) { passed++ })
	if err != nil {
		t.Fatal(err)
	}

	// Message shapes as sent by Docker 29 with the containerd image store.
	for _, e := range []Event{
		{Stage: "pull", Message: "Pulling nodiaque/steamcmd:enshrouded"},
		pull("enshrouded", "Pulling from nodiaque/steamcmd", 0, 0),
		pull("aaa", "Pulling fs layer", 0, 0),
		pull("bbb", "Pulling fs layer", 0, 0),
		pull("ccc", "Already exists", 0, 0),
		pull("aaa", "Downloading", 1000, 4000),
		pull("bbb", "Downloading", 500, 1000),
	} {
		emit(e)
	}
	job := tr.list()[0]
	want := PullStatus{Layers: 3, Done: 1, Downloaded: 1500, Total: 5000}
	if job.Pull != want {
		t.Errorf("mid-download = %+v, want %+v", job.Pull, want)
	}
	if job.Message != "Pulling nodiaque/steamcmd:enshrouded" || job.Stage != "pull" {
		t.Errorf("stage/message = %q/%q", job.Stage, job.Message)
	}

	emit(pull("aaa", "Download complete", 0, 0))
	emit(pull("bbb", "Download complete", 0, 0))
	emit(pull("aaa", "Extracting", 1, 0))
	job = tr.list()[0]
	want = PullStatus{Layers: 3, Done: 3, Downloaded: 5000, Total: 5000, Extracting: true}
	if job.Pull != want {
		t.Errorf("extracting = %+v, want %+v", job.Pull, want)
	}
	if passed != 10 {
		t.Errorf("passed on %d events, want 10", passed)
	}

	emit(Event{Stage: "warning", Message: "something odd"})
	emit(Event{Stage: "start", Message: "Starting container"})
	finish(nil)
	job = tr.list()[0]
	if job.Stage != "done" || job.Finished == nil || job.Message != "Starting container" {
		t.Errorf("finished job = %+v", job)
	}
}

func TestJobOnePerServer(t *testing.T) {
	tr := newJobTracker()
	noop := func(Event) {}
	_, finish, err := tr.start(Job{Kind: "update", Server: "valheim"}, noop)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.start(Job{Kind: "reconfigure", Server: "valheim"}, noop); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("second job on same server: err = %v", err)
	}
	if _, _, err := tr.start(Job{Kind: "install", Server: "other"}, noop); err != nil {
		t.Errorf("job on another server: %v", err)
	}

	finish(errors.New("boom"))
	jobs := tr.list()
	if jobs[0].Stage != "error" || jobs[0].Error != "boom" {
		t.Errorf("failed job = %+v", jobs[0])
	}
	if _, _, err := tr.start(Job{Kind: "update", Server: "valheim"}, noop); err != nil {
		t.Errorf("new job after the first finished: %v", err)
	}
}

func TestUpdateAllKeepsToAcceptedServers(t *testing.T) {
	game, other := gameServer("g1", "valheim"), gameServer("g2", "plex")
	fd := &labelDocker{containers: []docker.ContainerJSON{game, other}}
	srv := httptest.NewServer(fd)
	defer srv.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	m.updates.set("g1", UpdateStatus{State: UpdateAvailable})

	var events []Event
	keep := func(s Server) bool { return s.Name == "plex" }
	if err := m.UpdateAll(context.Background(), keep, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Message != "No updates available" {
		t.Errorf("a server it wasn't asked to keep was updated: %+v", events)
	}
}
