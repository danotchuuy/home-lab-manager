package backup

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Every server's backups: those with a plan or a run, only the ones the
// caller can see, with the running ones' progress and when each is next
// due, all without asking Docker about any server.
func TestAllBackups(t *testing.T) {
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	servers := &fakeServers{}
	s := New(store, servers, slog.New(slog.DiscardHandler))
	last := time.Now().Add(-time.Hour)
	if err := s.updatePlan("pal@node", func(p *Plan) {
		p.Folders, p.IntervalHours = []string{"/palworld/Saved"}, 6
		p.Last = &Run{Started: last, Finished: last.Add(time.Minute), Size: 100}
		p.LastOK = p.Last
	}); err != nil {
		t.Fatal(err)
	}
	s.updatePlan("valheim", func(p *Plan) { p.Folders = []string{"/config"} }) // no schedule
	s.updatePlan("secret", func(p *Plan) { p.Folders = []string{"/data"}; p.IntervalHours = 1 })
	s.updatePlan("empty", func(p *Plan) {}) // nothing chosen, never run
	if _, err := s.claim("pal@node", "backup", "manual"); err != nil {
		t.Fatal(err)
	}
	s.claim(configRunName, "backup", "schedule")

	can := func(name string) bool { return name != "secret" && name != "" }
	all, err := s.All(context.Background(), can, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers.calls) != 0 {
		t.Errorf("asked Docker: %v", servers.calls)
	}
	if all.Configured || all.Config != nil {
		t.Errorf("configured %v, config %+v", all.Configured, all.Config)
	}
	if len(all.Servers) != 2 || all.Servers[0].Server != "pal@node" || all.Servers[1].Server != "valheim" {
		t.Fatalf("servers %+v", all.Servers)
	}
	pal := all.Servers[0]
	if pal.Running == nil || pal.Running.Stage != "preparing" || pal.LastOK == nil || pal.Next == nil {
		t.Errorf("pal: %+v", pal)
	}
	if want := last.Add(6 * time.Hour); pal.Next != nil && !pal.Next.Equal(want) {
		t.Errorf("next %s, want %s", pal.Next, want)
	}
	if v := all.Servers[1]; v.Next != nil || v.Running != nil {
		t.Errorf("valheim: %+v", v)
	}

	// An admin sees hlm's own settings backup too.
	all, _ = s.All(context.Background(), func(string) bool { return true }, false)
	if all.Config == nil || len(all.Servers) != 3 {
		t.Errorf("admin: config %+v, %d servers", all.Config, len(all.Servers))
	}
}
