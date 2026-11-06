package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/idle"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// --- tags, favourites and bulk actions, for the server list ---

// userKey names the caller for their own preferences: their account (or
// single sign-on name, or API key); everyone shares one list when sign-in
// isn't required.
func userKey(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(rbac.Principal)
	if len(p.Subjects) > 0 {
		return p.Subjects[0].Kind + ":" + p.Subjects[0].Name
	}
	return "anonymous"
}

// decorate adds servers' game profiles, whether they're asleep, their tags
// and whether the caller has them as favourites.
func (s *server) decorate(ctx context.Context, list []manager.Server) []manager.Server {
	var asleep map[string]idle.Asleep
	if s.Idle != nil {
		asleep = s.Idle.AsleepSince()
	}
	for i := range list {
		list[i].GameProfile = gameprofile.Match(list[i].Image).Ref()
		if a, ok := asleep[list[i].Name]; ok && list[i].State != "running" {
			list[i].Asleep = &manager.Asleep{Since: a.Since, Minutes: a.Minutes}
		}
	}
	if s.Prefs == nil {
		return list
	}
	tags, _ := s.Prefs.Tags()
	favs, _ := s.Prefs.Favourites(userKey(ctx))
	fav := map[string]bool{}
	for _, f := range favs {
		fav[f] = true
	}
	for i := range list {
		list[i].Tags = tags[list[i].Name]
		list[i].Favourite = fav[list[i].Name]
	}
	return list
}

// presentOne is a server as the caller sees it, with its tags and whether
// they've pinned it.
func (s *server) presentOne(r *http.Request, srv manager.Server) manager.Server {
	return s.decorate(r.Context(), []manager.Server{s.present(s.access(r), srv)})[0]
}

func (s *server) setServerTags(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var in struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tags, err := s.Prefs.SetTags(name, in.Tags)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "tags", strings.Join(tags, ", "))
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func (s *server) setFavourite(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var in struct {
		Favourite bool `json:"favourite"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	list, err := s.Prefs.SetFavourite(userKey(r.Context()), name, in.Favourite)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if list == nil {
		list = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"favourites": list})
}

// bulkActions are what POST /api/servers/bulk can do, and the role each
// takes on every server.
var bulkActions = map[string]rbac.Role{
	"start": rbac.Operator, "stop": rbac.Operator, "restart": rbac.Operator, "update": rbac.Operator, "backup": rbac.Operator,
}

// bulkServers runs one action on several servers in turn, streaming each
// one's outcome (target: the server). Servers the caller may not act on are
// skipped and reported, as are ones they can't see (as "not found").
func (s *server) bulkServers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Servers []string `json:"servers"`
		Action  string   `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	need, ok := bulkActions[in.Action]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("the action is start, stop, restart, update or backup"))
		return
	}
	if len(in.Servers) == 0 || len(in.Servers) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("choose 1–200 servers"))
		return
	}
	access := s.access(r)
	audit.Add(r.Context(), "action", in.Action)
	audit.Add(r.Context(), "servers", strings.Join(in.Servers, ", "))
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		done, failed := 0, 0
		for _, name := range in.Servers {
			target := func(e manager.Event) { e.Target = name; emit(e) }
			fail := func(err error) {
				failed++
				target(manager.Event{Stage: "failed", Message: err.Error()})
			}
			switch role := access.Role(name); {
			case role == rbac.None:
				fail(manager.ErrNotFound)
				continue
			case !role.AtLeast(need):
				fail(errors.New("you can't do that to this server"))
				continue
			}
			target(manager.Event{Stage: "begin", Message: bulkVerb(in.Action) + "…"})
			var err error
			switch in.Action {
			case "start":
				err = s.Manager.Start(ctx, name)
			case "stop":
				err = s.Manager.Stop(ctx, name)
			case "restart":
				err = s.Manager.Restart(ctx, name)
			case "update":
				var srv manager.Server
				if srv, err = s.Manager.Get(ctx, name); err == nil && (srv.Update == nil || srv.Update.State != manager.UpdateAvailable) {
					target(manager.Event{Stage: "done", Message: "Already up to date"})
					done++
					continue
				}
				if err == nil {
					// Update's own "done" event is the server's.
					_, err = s.Manager.Update(ctx, name, func(e manager.Event) {
						if e.Stage == "done" {
							e.Message = "Updated"
						}
						target(e)
					})
					if err == nil {
						done++
						continue
					}
				}
			case "backup":
				if s.Backup == nil {
					err = errors.New("backups aren't set up")
				} else {
					err = s.Backup.Start(ctx, name)
				}
			}
			if err != nil {
				fail(err)
				continue
			}
			done++
			msg := map[string]string{"start": "Started", "stop": "Stopped", "restart": "Restarted", "backup": "Backup started"}[in.Action]
			target(manager.Event{Stage: "done", Message: msg})
		}
		summary := fmt.Sprintf("%s: %d done", strings.ToUpper(in.Action[:1])+in.Action[1:], done)
		if failed > 0 {
			summary += ", " + strconv.Itoa(failed) + " failed"
		}
		emit(manager.Event{Stage: "summary", Message: summary})
		return nil
	})
}

func bulkVerb(action string) string {
	return map[string]string{"start": "Starting", "stop": "Stopping", "restart": "Restarting", "update": "Updating", "backup": "Backing up"}[action]
}
