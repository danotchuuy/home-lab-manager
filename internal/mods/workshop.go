package mods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// Steam Workshop mods: the image downloads them, given their IDs in one of
// its variables. hlm changes that variable through the server's settings
// (which recreates the container), shows the items' names and update
// times from Steam, and puts the old list back if the server doesn't come
// back.

// ErrNoWorkshop is a server whose image hlm doesn't know a Workshop
// variable for.
var ErrNoWorkshop = errors.New("hlm doesn't know how this server's image takes Steam Workshop items")

// maxWorkshopItems bounds a list.
const maxWorkshopItems = 100

// WorkshopOverview is a server's Workshop items.
type WorkshopOverview struct {
	Game  string `json:"game"`
	AppID int    `json:"appId"`
	// Env is the variable with the items; NamesEnv the one with their mod
	// names, if the image has one.
	Env      string         `json:"env"`
	NamesEnv string         `json:"namesEnv,omitempty"`
	Items    []WorkshopItem `json:"items"`
	Running  bool           `json:"running"`
	Started  *time.Time     `json:"started,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	// Timeout is how long the server gets to come back after a change.
	Timeout time.Duration `json:"timeoutNs"`
}

// WorkshopItem is one item in the list.
type WorkshopItem struct {
	ID string `json:"id"`
	// Name is its mod name, for images that take those too.
	Name    string     `json:"name,omitempty"`
	Title   string     `json:"title,omitempty"`
	Updated *time.Time `json:"updated,omitempty"`
	Size    int64      `json:"size,omitempty"`
	// Missing says Steam has no such item (or it's private); WrongGame
	// that it's for another game.
	Missing   bool `json:"missing,omitempty"`
	WrongGame bool `json:"wrongGame,omitempty"`
	// Newer says it was updated on Steam after the server started: the
	// image gets the update when it next starts.
	Newer bool `json:"newer,omitempty"`
}

// WorkshopEntry is an item in a new list.
type WorkshopEntry struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type workshopState struct {
	info    docker.ContainerJSON
	profile *gameprofile.Profile
	img     gameprofile.WorkshopImage
	app     *catalog.App
	req     deploy.Request
	items   []WorkshopEntry
}

func splitList(v, sep string) []string {
	var out []string
	for _, p := range strings.Split(v, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Service) workshopState(ctx context.Context, server string) (*workshopState, error) {
	info, err := s.opts.Servers.Inspect(ctx, server)
	if err != nil {
		return nil, err
	}
	p := gameprofile.Match(info.Config.Image)
	if p == nil || p.Mods == nil {
		return nil, ErrNoWorkshop
	}
	img, ok := p.Mods.Workshop.For(info.Config.Image)
	if !ok {
		return nil, ErrNoWorkshop
	}
	app, req, err := s.opts.Servers.Config(ctx, server)
	if err != nil {
		return nil, err
	}
	st := &workshopState{info: info, profile: p, img: img, app: app, req: req}
	ids, _ := deploy.EnvValue(app, req, img.Env)
	var names []string
	if img.NamesEnv != "" {
		v, _ := deploy.EnvValue(app, req, img.NamesEnv)
		names = splitList(v, img.Separator)
	}
	for i, id := range splitList(ids, img.Separator) {
		e := WorkshopEntry{ID: id}
		if i < len(names) {
			e.Name = names[i]
		}
		st.items = append(st.items, e)
	}
	return st, nil
}

// Workshop lists a server's Workshop items, with what Steam says of them.
func (s *Service) Workshop(ctx context.Context, server string) (WorkshopOverview, error) {
	st, err := s.workshopState(ctx, server)
	if err != nil {
		return WorkshopOverview{}, err
	}
	ov := WorkshopOverview{Game: st.profile.Name, AppID: st.profile.Mods.Workshop.AppID, Env: st.img.Env, NamesEnv: st.img.NamesEnv,
		Items: []WorkshopItem{}, Running: st.info.State.Running, Timeout: st.profile.Mods.Timeout()}
	if s.opts.StartTimeout > 0 {
		ov.Timeout = s.opts.StartTimeout
	}
	var started time.Time
	if ov.Running {
		if t, err := time.Parse(time.RFC3339Nano, st.info.State.StartedAt); err == nil {
			started = t
			ov.Started = &t
		}
	}
	ids := make([]string, len(st.items))
	for i, e := range st.items {
		ids[i] = e.ID
	}
	var details map[string]WorkshopDetails
	if len(ids) > 0 && s.opts.Steam != nil {
		if details, err = s.opts.Steam.Details(ctx, ids); err != nil {
			ov.Warnings = append(ov.Warnings, "Couldn't get the items' names from Steam: "+err.Error())
		}
	}
	if img := st.img; img.NamesEnv != "" {
		names, _ := deploy.EnvValue(st.app, st.req, img.NamesEnv)
		if n := len(splitList(names, img.Separator)); n != len(st.items) {
			ov.Warnings = append(ov.Warnings, fmt.Sprintf("%s has %d mod names for %d Workshop items in %s; saving here lines them up.",
				img.NamesEnv, n, len(st.items), img.Env))
		}
	}
	for _, e := range st.items {
		it := WorkshopItem{ID: e.ID, Name: e.Name}
		if d, ok := details[e.ID]; ok {
			if !d.Found {
				it.Missing = true
			} else {
				it.Title, it.Size = d.Title, d.Size
				it.WrongGame = d.AppID != ov.AppID
				if !d.Updated.IsZero() {
					u := d.Updated
					it.Updated = &u
					it.Newer = !started.IsZero() && u.After(started)
				}
			}
		}
		ov.Items = append(ov.Items, it)
	}
	return ov, nil
}

// checkWorkshop checks a new list.
func checkWorkshop(img gameprofile.WorkshopImage, items []WorkshopEntry) ([]WorkshopEntry, error) {
	if len(items) > maxWorkshopItems {
		return nil, fmt.Errorf("at most %d Workshop items", maxWorkshopItems)
	}
	seen := map[string]bool{}
	out := make([]WorkshopEntry, len(items))
	for i, e := range items {
		id, ok := WorkshopID(e.ID)
		if !ok {
			return nil, fmt.Errorf("%q isn't a Workshop item's ID or link", e.ID)
		}
		if seen[id] {
			return nil, fmt.Errorf("%s is in the list twice", id)
		}
		seen[id] = true
		name := strings.TrimSpace(e.Name)
		switch {
		case img.NamesEnv == "" && name != "":
			return nil, errors.New("this image doesn't take mod names")
		case img.NamesEnv != "" && name == "":
			return nil, fmt.Errorf("%s needs its mod name (the mod's ID inside the item; its Workshop page usually says it)", id)
		case name != "" && (!modNameRE.MatchString(name) || strings.Contains(name, img.Separator)):
			return nil, fmt.Errorf("%q isn't a mod name hlm can put in the list", name)
		}
		out[i] = WorkshopEntry{ID: id, Name: name}
	}
	return out, nil
}

// ApplyWorkshop replaces a server's Workshop list: the server is recreated
// with the new variable, and if it was running, hlm waits for it to answer
// its status query; if it doesn't, the old list is put back.
func (s *Service) ApplyWorkshop(ctx context.Context, server, who string, items []WorkshopEntry, emit func(manager.Event)) (res Result, err error) {
	st, err := s.workshopState(ctx, server)
	if err != nil {
		return res, err
	}
	if items, err = checkWorkshop(st.img, items); err != nil {
		return res, err
	}
	if slices.Equal(items, st.items) {
		return res, errors.New("nothing to change")
	}
	if why := s.opts.Busy(server); why != "" {
		return res, errors.New(why)
	}
	appID := st.profile.Mods.Workshop.AppID
	var added []string
	for _, e := range items {
		if !slices.ContainsFunc(st.items, func(o WorkshopEntry) bool { return o.ID == e.ID }) {
			added = append(added, e.ID)
		}
	}
	if len(added) > 0 {
		if s.opts.Steam == nil {
			return res, errors.New("hlm can't check the items with Steam")
		}
		details, err := s.opts.Steam.Details(ctx, added)
		if err != nil {
			return res, fmt.Errorf("couldn't check the new items with Steam: %w", err)
		}
		for _, id := range added {
			d, ok := details[id]
			switch {
			case !ok || !d.Found:
				return res, fmt.Errorf("Steam has no Workshop item %s (or it's private)", id)
			case d.AppID != appID:
				return res, fmt.Errorf("%s (%s) is for another game, not %s", d.Title, id, st.profile.Name)
			case d.Banned:
				return res, fmt.Errorf("%s (%s) is banned on the Workshop", d.Title, id)
			}
		}
	}

	ids, names := make([]string, len(items)), make([]string, len(items))
	for i, e := range items {
		ids[i], names[i] = e.ID, e.Name
	}
	newReq := st.req
	newReq.Values = cloneMap(st.req.Values)
	deploy.SetEnv(st.app, &newReq, st.img.Env, strings.Join(ids, st.img.Separator))
	if st.img.NamesEnv != "" {
		deploy.SetEnv(st.app, &newReq, st.img.NamesEnv, strings.Join(names, st.img.Separator))
	}
	timeout := st.profile.Mods.Timeout()
	if s.opts.StartTimeout > 0 {
		timeout = s.opts.StartTimeout
	}
	running := st.info.State.Running
	// The recreate's own end isn't the change's: its done and error are
	// steps here, and the change ends after the wait.
	inner := func(e manager.Event) {
		if e.Stage == "done" || e.Stage == "error" {
			e.Stage = "apply"
		}
		emit(e)
	}
	s.opts.Log.Info("changing Workshop items", "server", server, "by", who, "items", ids)
	if _, err := s.opts.Servers.Reconfigure(ctx, server, newReq, inner); err != nil {
		return res, fmt.Errorf("couldn't apply the new list: %w", err)
	}
	if !running {
		res.Warning = "The server is stopped, so the items download when it next starts; hlm couldn't check them."
		return res, nil
	}
	emit(manager.Event{Stage: "wait", Message: fmt.Sprintf("Waiting for %s to download the items and answer (up to %s)", server, timeout)})
	warning, cause := s.waitUp(ctx, server, timeout)
	if cause == nil {
		res.Warning = warning
		return res, nil
	}

	s.opts.Log.Warn("Workshop change failed; putting the old list back", "server", server, "err", cause)
	emit(manager.Event{Stage: "rollback", Message: fmt.Sprintf("That didn't work (%v); putting the old list back", cause)})
	res.RolledBack = true
	err = fmt.Errorf("the server didn't come back with the new Workshop list (%v), so hlm put the old list back", cause)
	bctx := context.WithoutCancel(ctx)
	_, back := s.opts.Servers.Reconfigure(bctx, server, st.req, inner)
	if back == nil {
		// It may have stopped (crashed) before it was recreated, and a
		// stopped server is recreated stopped; it was running before.
		if info, ierr := s.opts.Servers.Inspect(bctx, server); ierr == nil && !info.State.Running {
			emit(manager.Event{Stage: "start", Message: "Starting " + server + " again with the old list"})
			back = s.opts.Servers.Start(bctx, server)
		}
	}
	if back != nil {
		err = fmt.Errorf("the server didn't come back with the new Workshop list (%v), and putting the old one back failed: %v", cause, back)
	} else if _, again := s.waitUp(bctx, server, timeout); again != nil {
		err = fmt.Errorf("the server didn't come back with the new Workshop list (%v). hlm put the old list back, but it doesn't start with that either (%v)", cause, again)
	}
	if s.opts.Notifier != nil {
		s.opts.Notifier.Notify(notify.Event{Kind: notify.KindModsFailed, Severity: notify.Error,
			Title:   "A Workshop change on " + server + " was undone",
			Message: err.Error(), Server: server, Link: "/servers/" + server + "?tab=mods",
			Fields: map[string]string{"Items": strings.Join(ids, ", ")}})
	}
	return res, err
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
