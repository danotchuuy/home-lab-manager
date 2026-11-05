package hosts

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestAgentEnrolment(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	var got []manager.RemoteHost
	hub := agent.NewHub(slog.New(slog.DiscardHandler))
	r := New(Options{Secrets: store, Agents: hub, OnChange: func(h []manager.RemoteHost) { got = h }, Log: slog.New(slog.DiscardHandler)})
	ctx := context.Background()

	if _, err := r.CreateEnrolment(Host{ID: "nas", AppdataRoot: "/a"}); err == nil || !strings.Contains(err.Error(), "address") {
		t.Errorf("no address: %v", err)
	}
	first, err := r.CreateEnrolment(Host{ID: "nas", Name: "NAS", AppdataRoot: "/mnt/user/appdata/", Address: "192.168.1.20"})
	if err != nil {
		t.Fatal(err)
	}
	// A new token for the same host replaces the first.
	e, err := r.CreateEnrolment(Host{ID: "nas", Name: "NAS", AppdataRoot: "/mnt/user/appdata/", Address: "192.168.1.20"})
	if err != nil || !strings.HasPrefix(e.Token, "hlme_") || e.Host.Kind != KindAgent || e.Host.AppdataRoot != "/mnt/user/appdata" ||
		time.Until(e.Expires) > EnrolmentTTL || time.Until(e.Expires) < EnrolmentTTL-time.Minute {
		t.Fatalf("enrolment %+v %v", e, err)
	}
	if _, _, err := r.Enrol(first.Token); !errors.Is(err, ErrBadToken) {
		t.Errorf("replaced token: %v", err)
	}
	if _, _, err := r.Enrol("hlme_nope"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token: %v", err)
	}
	h, cred, err := r.Enrol(e.Token)
	if err != nil || h.ID != "nas" || h.Kind != KindAgent || h.Created.IsZero() {
		t.Fatalf("enrol: %+v %v", h, err)
	}
	// Single use.
	if _, _, err := r.Enrol(e.Token); !errors.Is(err, ErrBadToken) {
		t.Errorf("used twice: %v", err)
	}
	if len(got) != 1 || got[0].ID != "nas" || got[0].Address != "192.168.1.20" {
		t.Errorf("manager hosts: %+v", got)
	}
	// Only the credential's hash is kept.
	var stored string
	store.Get(secretAgentPrefix+"nas", &stored)
	id, secret, ok := agent.ParseCredential(cred)
	if !ok || id != "nas" || stored == "" || strings.Contains(stored, secret) {
		t.Fatalf("credential %q, stored %q", cred, stored)
	}
	if !r.authenticateAgent("nas", secret) || r.authenticateAgent("nas", secret+"x") || r.authenticateAgent("other", secret) {
		t.Error("authentication")
	}
	// Not connected: Docker calls say so.
	if _, err := r.Check(ctx, "nas"); err == nil || !strings.Contains(err.Error(), "agent isn't connected") {
		t.Errorf("check: %v", err)
	}
	if st := r.AgentStatus("nas"); st == nil || st.Connected {
		t.Errorf("status %+v", st)
	}

	// It can be renamed and moved while away, but stays an agent's host.
	if h, err := r.Save(ctx, Input{Host: Host{ID: "nas", Kind: KindAgent, Name: "Big NAS", AppdataRoot: "/data", Address: "nas.lan"}}, false); err != nil || h.Name != "Big NAS" {
		t.Errorf("update: %+v %v", h, err)
	}
	if _, err := r.Save(ctx, Input{Host: Host{ID: "nas", Kind: KindTCP, URL: "tcp://nas.lan", AppdataRoot: "/data"}}, false); err == nil || !strings.Contains(err.Error(), "stays one") {
		t.Errorf("switch kind: %v", err)
	}
	if _, err := r.Save(ctx, Input{Host: Host{ID: "pi", Kind: KindAgent, AppdataRoot: "/data", Address: "pi.lan"}}, true); err == nil || !strings.Contains(err.Error(), "enrolling") {
		t.Errorf("create without enrolling: %v", err)
	}
	if _, err := r.CreateEnrolment(Host{ID: "nas", AppdataRoot: "/a", Address: "x"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("enrol an existing host: %v", err)
	}

	// Removed: the credential stops working.
	if err := r.Delete("nas"); err != nil {
		t.Fatal(err)
	}
	if r.authenticateAgent("nas", secret) {
		t.Error("removed host's agent still accepted")
	}

	// Expired tokens don't work.
	e, _ = r.CreateEnrolment(Host{ID: "pi", AppdataRoot: "/a", Address: "pi.lan"})
	var list []enrolment
	store.Get(secretEnrolments, &list)
	list[0].Expires = time.Now().Add(-time.Second)
	store.Set(secretEnrolments, list)
	if _, _, err := r.Enrol(e.Token); !errors.Is(err, ErrBadToken) {
		t.Errorf("expired: %v", err)
	}

	// Without a hub, there are no agents.
	r2 := New(Options{Secrets: store, Log: slog.New(slog.DiscardHandler)})
	if _, err := r2.CreateEnrolment(Host{ID: "x", AppdataRoot: "/a", Address: "x"}); err == nil {
		t.Error("enrolment without a hub")
	}
}
