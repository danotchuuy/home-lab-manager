package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/health"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// --- Health checks and crash loops ---

type healthDefaults struct {
	Exits       int `json:"exits"`
	Minutes     int `json:"minutes"`
	Interval    int `json:"interval"`
	Timeout     int `json:"timeout"`
	Retries     int `json:"retries"`
	StartPeriod int `json:"startPeriod"`
}

type healthStatus struct {
	Settings health.Settings `json:"settings"`
	Defaults healthDefaults  `json:"defaults"`
	// Health is Docker's view: starting, healthy or unhealthy; empty when
	// the container has no check (or isn't running).
	Health      string             `json:"health,omitempty"`
	CrashLoop   *manager.CrashLoop `json:"crashLoop,omitempty"`
	RecentExits int                `json:"recentExits"`
	// CurrentCheck is the container's check now (operators only): the
	// image's, the template's, or hlm's.
	CurrentCheck *docker.Healthcheck `json:"currentCheck,omitempty"`
	// Custom says the container's check is the one hlm set.
	Custom bool `json:"custom"`
	// Applied says the container has the check the settings ask for; a
	// change waits for it to be recreated.
	Applied        bool       `json:"applied"`
	UnhealthySince *time.Time `json:"unhealthySince,omitempty"`
	// QueryAvailable says the game's status query works for this server,
	// so it can be the health signal.
	QueryAvailable bool `json:"queryAvailable"`
}

func (s *server) serverHealth(w http.ResponseWriter, r *http.Request) {
	if s.Health == nil {
		writeError(w, http.StatusNotFound, errors.New("health settings aren't available"))
		return
	}
	info, err := s.Manager.Inspect(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	name := strings.TrimPrefix(info.Name, "/")
	set, err := s.Health.Get(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := healthStatus{
		Settings: set,
		Defaults: healthDefaults{
			Exits: manager.DefaultCrashLoopExits, Minutes: int(manager.DefaultCrashLoopWindow / time.Minute),
			Interval: health.DefaultInterval, Timeout: health.DefaultTimeout, Retries: health.DefaultRetries,
			StartPeriod: health.DefaultStartPeriod,
		},
		RecentExits:  s.Manager.RecentExits(name),
		CurrentCheck: info.Config.Healthcheck,
		Custom:       info.Config.Labels[manager.LabelHealth] == "custom",
		Applied:      s.Manager.HealthcheckApplied(info, name),
	}
	if info.State.Running && info.State.Health != nil {
		out.Health = info.State.Health.Status
	}
	if cl, ok := s.Manager.CrashLoopOf(name); ok {
		out.CrashLoop = &cl
	}
	if s.HealthMonitor != nil {
		if since, ok := s.HealthMonitor.UnhealthySince(name); ok {
			out.UnhealthySince = &since
		}
	}
	if s.Query != nil {
		st := s.Query.Status(name, false)
		// Chosen, or found while it ran.
		proto := st.Settings.Protocol
		out.QueryAvailable = (proto != gamequery.ProtoAuto && proto != gamequery.ProtoOff) ||
			(proto == gamequery.ProtoAuto && st.Method != nil)
	}
	// Commands can carry passwords; viewers don't see them.
	if !s.access(r).Can(name, rbac.Operator) {
		out.Settings.Check.Command = ""
		out.CurrentCheck = nil
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) setServerHealth(w http.ResponseWriter, r *http.Request) {
	if s.Health == nil {
		writeError(w, http.StatusNotFound, errors.New("health settings aren't available"))
		return
	}
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var set health.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&set); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	set, err := s.Health.Set(name, set)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "check", set.Check.Kind)
	s.serverHealth(w, r)
}

// testServerHealth runs a health check in a running server once: the one
// sent, or the container's own.
func (s *server) testServerHealth(w http.ResponseWriter, r *http.Request) {
	info, err := s.Manager.Inspect(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	var body struct {
		Check *health.Check `json:"check"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var hc *docker.Healthcheck
	if body.Check != nil && body.Check.Kind != health.CheckImage {
		set, err := health.Settings{Check: *body.Check}.Validate()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		hc = set.Check.Healthcheck()
		audit.Add(r.Context(), "check", set.Check.Kind)
	} else {
		hc = info.Config.Healthcheck
		audit.Add(r.Context(), "check", "current")
	}
	if hc == nil || len(hc.Test) == 0 || hc.Test[0] == "NONE" {
		writeError(w, http.StatusBadRequest, errors.New("there's no health check to run"))
		return
	}
	code, output, err := s.Manager.RunCheck(r.Context(), info.ID, *hc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"healthy": code == 0, "exitCode": code, "output": output})
}
