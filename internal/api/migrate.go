package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/transfer"
)

// --- Moving a server to another Docker host ---

// migratePreview is a move's preview, plus what follows the server that
// isn't its own settings: port forwards, web addresses, the proxy's relay.
type migratePreview struct {
	transfer.MigratePreview
	Follows []string `json:"follows"`
}

func (s *server) previewMigrate(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var body struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	p, err := s.Transfer.PreviewMigrate(ctx, name, body.Host)
	if err != nil {
		writeAdoptError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, migratePreview{MigratePreview: p, Follows: s.migrateFollows(ctx, name, body.Host)})
}

// migrateFollows says what's pointed at a server's new host when it moves.
func (s *server) migrateFollows(ctx context.Context, name, host string) []string {
	out := []string{}
	where := s.Manager.HostName(host)
	if s.Forwarding != nil && s.Forwarding.Configured() {
		if st, err := s.Forwarding.Status(ctx, name); err == nil {
			n := 0
			for _, r := range st.Rules {
				if r.State != "missing" {
					n++
				}
			}
			if n > 0 {
				out = append(out, fmt.Sprintf("Its %d port forward(s) on UniFi are pointed at %s.", n, where))
			}
		}
	}
	if s.Tunnel != nil {
		if hs := s.Tunnel.ServerHostnames(name); len(hs) > 0 {
			out = append(out, "Its web addresses ("+strings.Join(hs, ", ")+") are routed to "+where+".")
		}
	}
	if s.Proxy != nil && s.Proxy.Relayed(name) {
		out = append(out, "The proxy server relays it to "+where+".")
	}
	out = append(out, "Its server groups, schedules, backup plan and history, players, tags, favourites, status page entry, notifications and other settings go with it.")
	return out
}

func (s *server) migrateServer(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var opts transfer.MigrateOptions
	if !readExport(w, r, &opts) {
		return
	}
	audit.Add(r.Context(), "host", opts.Host)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		_, err := s.Transfer.Migrate(ctx, name, opts, s.switchover, emit)
		return err
	})
}

// switchover moves everything kept by a moved server's name to its new
// one, and points its port forwards, web addresses and relay at its new
// host. What fails is reported, and the rest still moves.
func (s *server) switchover(ctx context.Context, from, to string, emit func(manager.Event)) {
	emit(manager.Event{Stage: "copy", Message: "Moving " + from + "'s settings to " + to})
	move := func(what string, err error) {
		if err != nil {
			s.Log.Warn("move a server's "+what, "from", from, "to", to, "err", err)
			emit(manager.Event{Stage: "warning", Message: fmt.Sprintf("Couldn't move its %s: %v", what, err)})
		}
	}
	move("server groups", s.Groups.RenameServer(from, to))
	if s.Backup != nil {
		move("backup plan", s.Backup.RenameServer(from, to))
	}
	if s.Schedule != nil {
		move("scheduled tasks", s.Schedule.RenameServer(from, to))
	}
	if s.AutoUpdate != nil {
		move("update policy", s.AutoUpdate.RenameServer(from, to))
	}
	if s.Query != nil {
		move("status query settings", s.Query.RenameServer(from, to))
	}
	if s.Console != nil {
		move("console settings", s.Console.RenameServer(from, to))
	}
	if s.ConfigFiles != nil {
		move("pinned files", s.ConfigFiles.RenameServer(from, to))
	}
	if s.Health != nil {
		move("health settings", s.Health.RenameServer(from, to))
	}
	if s.Notify != nil {
		move("notification targets", s.Notify.RenameServer(from, to))
	}
	if s.Players != nil {
		move("player records", s.Players.RenameServer(from, to))
	}
	if s.Mods != nil {
		s.Mods.ForgetServer(from) // update checks are a cache; they're made again
	}
	if s.RestoreTests != nil {
		move("restore tests", s.RestoreTests.RenameServer(from, to))
	}
	if s.Idle != nil {
		move("idle settings", s.Idle.RenameServer(from, to))
	}
	if s.StatusPage != nil {
		move("status page entry", s.StatusPage.RenameServer(from, to))
	}
	if s.Prefs != nil {
		move("tags and favourites", s.Prefs.RenameServer(from, to))
	}

	where := s.Manager.HostName(manager.HostOf(to))
	if s.Forwarding != nil {
		move("port forwarding choices", s.Forwarding.RenameServer(from, to))
		if s.Forwarding.Configured() {
			removed, err := s.Forwarding.Remove(ctx, from)
			move("port forwards", err)
			if st, err := s.Forwarding.Sync(ctx, to); err != nil {
				move("port forwards", err)
			} else if removed > 0 || len(st.Rules) > 0 {
				emit(manager.Event{Stage: "copy", Message: "Port forwards on UniFi now go to " + st.Target + " on " + where})
			}
		}
	}
	if s.Tunnel != nil {
		hs := s.Tunnel.ServerHostnames(from)
		if err := s.Tunnel.RenameServer(ctx, from, to); err != nil {
			move("web addresses", err)
		} else if len(hs) > 0 {
			emit(manager.Event{Stage: "copy", Message: strings.Join(hs, ", ") + " now go to " + where})
		}
	}
	if s.Proxy != nil {
		if relayed, err := s.Proxy.RenameServer(ctx, from, to); err != nil {
			move("relay on the proxy server", err)
		} else if relayed {
			emit(manager.Event{Stage: "copy", Message: "The proxy server now relays it to " + where})
		}
	}
}
