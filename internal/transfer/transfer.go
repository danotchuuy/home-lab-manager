// Package transfer exports a server's settings to a file, imports them as
// a new server (on this hlm or another), and clones a server, with or
// without its data.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/autoupdate"
	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/configfiles"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/health"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/schedule"
)

// Version is the export format's version; files from a newer one are
// refused.
const Version = 1

// AppdataVar stands for the appdata root in exported paths, so a server
// moves to another host's root.
const AppdataVar = "${APPDATA}"

// Export is a server's settings, as a file (<server>.hlm.json).
type Export struct {
	HLMExport  int       `json:"hlmExport"`
	Exported   time.Time `json:"exported"`
	HLMVersion string    `json:"hlmVersion,omitempty"`
	// Server is the exported server's name.
	Server string `json:"server"`
	// Template is the server's template as the catalog has it, so the
	// server can be installed where the template isn't in the catalog.
	Template *catalog.App `json:"template"`
	// Request is the server's settings, with paths under the appdata root
	// starting with AppdataVar.
	Request deploy.Request `json:"request"`
	// Secrets says whether passwords and keys are included; when they
	// aren't, Omitted lists the settings left blank (by item ID, or
	// deploy.HiddenExtraParams and HiddenPostArgs).
	Secrets  bool     `json:"secrets"`
	Omitted  []string `json:"omitted,omitempty"`
	Settings Settings `json:"settings"`
}

// Settings are the server's settings kept outside its container.
type Settings struct {
	BackupPlan   *BackupPlan         `json:"backupPlan,omitempty"`
	Tasks        []schedule.Task     `json:"tasks,omitempty"`
	UpdatePolicy *autoupdate.Policy  `json:"updatePolicy,omitempty"`
	Query        *gamequery.Settings `json:"query,omitempty"`
	Console      *console.Settings   `json:"console,omitempty"`
	ConfigFiles  []configfiles.File  `json:"configFiles,omitempty"`
	Health       *health.Settings    `json:"health,omitempty"`
}

// BackupPlan is a backup plan without its history.
type BackupPlan struct {
	Folders       []string `json:"folders"`
	IntervalHours int      `json:"intervalHours"`
	Keep          int      `json:"keep"`
	StopServer    bool     `json:"stopServer"`
}

// Summary describes the settings in words, for the import and clone forms.
func (s Settings) Summary() []string {
	var out []string
	if s.BackupPlan != nil {
		out = append(out, "backup plan")
	}
	switch n := len(s.Tasks); {
	case n == 1:
		out = append(out, "1 scheduled task")
	case n > 1:
		out = append(out, fmt.Sprintf("%d scheduled tasks", n))
	}
	if s.UpdatePolicy != nil {
		out = append(out, "automatic updates")
	}
	if s.Query != nil {
		out = append(out, "status query settings")
	}
	if s.Console != nil {
		out = append(out, "console settings")
	}
	if len(s.ConfigFiles) > 0 {
		out = append(out, "pinned settings files")
	}
	if s.Health != nil {
		out = append(out, "health settings")
	}
	return out
}

// Manager is what the service needs from the server manager.
type Manager interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	List(ctx context.Context) ([]manager.Server, error)
	Config(ctx context.Context, id string) (*catalog.App, deploy.Request, error)
	App(id string) (*catalog.App, bool)
	AppdataRoot() string
	Install(ctx context.Context, req deploy.Request, emit func(manager.Event)) (manager.Server, error)
	Create(ctx context.Context, req deploy.Request, emit func(manager.Event)) (manager.Server, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	CopyData(ctx context.Context, fromID, toID string, folders []string, progress func(files.ArchiveStats)) (files.ArchiveStats, error)
	HostPortsInUse(ctx context.Context) (map[string]bool, error)
	Networks(ctx context.Context) ([]manager.NetworkInfo, error)
}

// Catalog is where templates come from.
type Catalog interface {
	Get(id string) (*catalog.App, bool)
	AddCustom(app *catalog.App) error
}

// Backups are servers' backup plans, and backups to restore from.
type Backups interface {
	Plans() (map[string]backup.Plan, error)
	SetPlan(ctx context.Context, id string, in backup.Plan) (backup.Plan, error)
	RestoreInto(ctx context.Context, from, key, toID string, progress func(files.ArchiveStats)) (backup.Run, error)
	IsBackupOf(name, key string) bool
}

// Tasks are servers' scheduled tasks.
type Tasks interface {
	Tasks(server string, withCommands bool) (schedule.ServerTasks, error)
	Save(server string, in schedule.Task) (schedule.Task, error)
}

// Updates are servers' update policies.
type Updates interface {
	Status(server string) (autoupdate.ServerStatus, error)
	SetServerPolicy(server string, p autoupdate.Policy) (autoupdate.Policy, error)
}

// Query is servers' status query settings.
type Query interface {
	Status(name string, names bool) gamequery.Status
	SetSettings(name string, set gamequery.Settings) (gamequery.Settings, error)
}

// Console is servers' console settings.
type Console interface {
	Settings(name string) (console.Settings, error)
	Save(ctx context.Context, id string, in console.Input) (console.Status, error)
}

// ConfigFiles are servers' pinned settings files.
type ConfigFiles interface {
	Pinned(server string) ([]configfiles.File, error)
	SetPinned(server string, in []configfiles.File) ([]configfiles.File, error)
}

// Health is servers' health settings.
type Health interface {
	Get(server string) (health.Settings, error)
	Set(server string, set health.Settings) (health.Settings, error)
}

// Options configures a Service. Only Manager and Catalog are required;
// settings kept by a service that's nil aren't exported or imported.
type Options struct {
	Manager     Manager
	Catalog     Catalog
	Backups     Backups
	Tasks       Tasks
	Updates     Updates
	Query       Query
	Console     Console
	ConfigFiles ConfigFiles
	Health      Health
	Version     string // hlm's, written into exports
	Log         *slog.Logger
}

// Service exports, imports and clones servers.
type Service struct{ opts Options }

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{opts: o}
}

// ValidName matches server (container) names.
var ValidName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// --- Export ---

// Export collects a server's settings. Without secrets, settings that look
// like passwords or keys are left blank and listed in Omitted, and the
// console's password is left out.
func (s *Service) Export(ctx context.Context, id string, secrets bool) (Export, error) {
	info, err := s.opts.Manager.Inspect(ctx, id)
	if err != nil {
		return Export{}, err
	}
	name := strings.TrimPrefix(info.Name, "/")
	app, req, err := s.opts.Manager.Config(ctx, id)
	if err != nil {
		return Export{}, err
	}
	if req.TemplateID == "" {
		return Export{}, errors.New("this server has no saved settings to export")
	}
	raw, ok := s.opts.Catalog.Get(req.TemplateID)
	if !ok {
		return Export{}, fmt.Errorf("template %q is no longer in the catalog", req.TemplateID)
	}
	req.Name = name
	// The live restart policy: start on boot may have been switched since.
	if p := info.HostConfig.RestartPolicy.Name; p != "" {
		req.RestartPolicy = p
	}
	exp := Export{HLMExport: Version, Exported: time.Now().UTC(), HLMVersion: s.opts.Version, Server: name,
		Template: raw, Secrets: secrets}
	if !secrets {
		hadExtraParams, hadPostArgs := req.ExtraParams != nil, req.PostArgs != nil
		var hidden []string
		_, req, hidden = deploy.HideSecrets(app, req)
		for _, h := range hidden {
			if (h == deploy.HiddenExtraParams && !hadExtraParams) || (h == deploy.HiddenPostArgs && !hadPostArgs) {
				continue // the template's own, not this server's
			}
			exp.Omitted = append(exp.Omitted, h)
		}
	}
	req, err = relativePaths(app, req, s.opts.Manager.AppdataRoot())
	if err != nil {
		return Export{}, err
	}
	exp.Request = req
	exp.Settings = s.settings(name, secrets)
	return exp, nil
}

// relativePaths writes every path setting into the request, with the
// appdata root as AppdataVar.
func relativePaths(app *catalog.App, req deploy.Request, root string) (deploy.Request, error) {
	items, err := deploy.Items(app, req)
	if err != nil {
		return req, err
	}
	req.Values = maps.Clone(req.Values)
	if req.Values == nil {
		req.Values = map[string]string{}
	}
	req.Extra = slices.Clone(req.Extra)
	root = strings.TrimRight(root, "/")
	for _, it := range items {
		if it.Type != catalog.TypePath || it.Effective == "" || root == "" {
			continue
		}
		if rel, ok := under(it.Effective, root); ok {
			setValue(&req, it.ID, AppdataVar+rel)
		}
	}
	return req, nil
}

// setValue sets an item's value: in the item itself for one the user
// added (the settings form edits those there), or else in Values.
func setValue(req *deploy.Request, id, v string) {
	if n, ok := extraIndex(id); ok && n < len(req.Extra) {
		req.Extra[n].Value = v
		delete(req.Values, id)
		return
	}
	req.Values[id] = v
}

// extraIndex reads an extra item's ID ("extra:2").
func extraIndex(id string) (int, bool) {
	rest, ok := strings.CutPrefix(id, "extra:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 0
}

// under reports whether p is root or inside it, and what follows root.
func under(p, root string) (string, bool) {
	if p == root {
		return "", true
	}
	if rest, ok := strings.CutPrefix(p, root+"/"); ok {
		return "/" + rest, true
	}
	return "", false
}

// settings collects a server's settings from the other services.
func (s *Service) settings(name string, secrets bool) Settings {
	var out Settings
	if s.opts.Backups != nil {
		if plans, err := s.opts.Backups.Plans(); err == nil {
			if p, ok := plans[name]; ok && (len(p.Folders) > 0 || p.IntervalHours > 0) {
				out.BackupPlan = &BackupPlan{Folders: p.Folders, IntervalHours: p.IntervalHours, Keep: p.Keep, StopServer: p.StopServer}
			}
		}
	}
	if s.opts.Tasks != nil {
		if st, err := s.opts.Tasks.Tasks(name, true); err == nil {
			for _, t := range st.Tasks {
				t.ID, t.Next, t.Anchor, t.Runs = "", nil, time.Time{}, nil
				out.Tasks = append(out.Tasks, t)
			}
		}
	}
	if s.opts.Updates != nil {
		if st, err := s.opts.Updates.Status(name); err == nil && st.Policy != (autoupdate.Policy{}) {
			p := st.Policy
			out.UpdatePolicy = &p
		}
	}
	if s.opts.Query != nil {
		if set := s.opts.Query.Status(name, false).Settings; set != (gamequery.Settings{}) {
			out.Query = &set
		}
	}
	if s.opts.Console != nil {
		if set, err := s.opts.Console.Settings(name); err == nil && set.Mode != console.ModeNone {
			if !secrets {
				set.Password = ""
			}
			out.Console = &set
		}
	}
	if s.opts.ConfigFiles != nil {
		if pins, err := s.opts.ConfigFiles.Pinned(name); err == nil && len(pins) > 0 {
			out.ConfigFiles = pins
		}
	}
	if s.opts.Health != nil {
		if set, err := s.opts.Health.Get(name); err == nil && !isZeroHealth(set) {
			out.Health = &set
		}
	}
	return out
}

func isZeroHealth(set health.Settings) bool {
	z := health.Settings{}
	return set.CrashLoop == z.CrashLoop && set.Check.Kind == "" && set.RestartAfter == 0 && set.Signal == ""
}

// apply gives a new server the settings, reporting each that fails as a
// warning: the server itself is already installed.
func (s *Service) apply(ctx context.Context, name string, set Settings, emit func(manager.Event)) {
	warn := func(what string, err error) {
		emit(manager.Event{Stage: "warning", Message: fmt.Sprintf("Couldn't copy the %s: %v", what, err)})
	}
	if set.BackupPlan != nil && s.opts.Backups != nil {
		p := set.BackupPlan
		if _, err := s.opts.Backups.SetPlan(ctx, name, backup.Plan{Folders: p.Folders, IntervalHours: p.IntervalHours,
			Keep: p.Keep, StopServer: p.StopServer}); err != nil {
			warn("backup plan", err)
		}
	}
	if s.opts.Tasks != nil {
		for _, t := range set.Tasks {
			t.ID, t.Runs = "", nil
			if _, err := s.opts.Tasks.Save(name, t); err != nil {
				warn("scheduled task "+t.Name, err)
			}
		}
	}
	if set.UpdatePolicy != nil && s.opts.Updates != nil {
		if _, err := s.opts.Updates.SetServerPolicy(name, *set.UpdatePolicy); err != nil {
			warn("update policy", err)
		}
	}
	if set.Query != nil && s.opts.Query != nil {
		if _, err := s.opts.Query.SetSettings(name, *set.Query); err != nil {
			warn("status query settings", err)
		}
	}
	if set.Console != nil && s.opts.Console != nil {
		if _, err := s.opts.Console.Save(ctx, name, console.Input{Settings: *set.Console}); err != nil {
			warn("console settings", err)
		}
	}
	if len(set.ConfigFiles) > 0 && s.opts.ConfigFiles != nil {
		if _, err := s.opts.ConfigFiles.SetPinned(name, set.ConfigFiles); err != nil {
			warn("pinned settings files", err)
		}
	}
	if set.Health != nil && s.opts.Health != nil {
		if _, err := s.opts.Health.Set(name, *set.Health); err != nil {
			warn("health settings", err)
		}
	}
	if sum := set.Summary(); len(sum) > 0 {
		emit(manager.Event{Stage: "settings", Message: "Copied its " + joinWords(sum)})
	}
}

func joinWords(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}

// --- Import ---

// Preview is a server about to be imported or cloned: the settings form,
// filled in, and what to look out for.
type Preview struct {
	// App is the template (localized), for the settings form.
	App     *catalog.App   `json:"app"`
	Request deploy.Request `json:"request"`
	// Omitted names the settings that were left out of the file.
	Omitted  []string `json:"omitted,omitempty"`
	Warnings []string `json:"warnings"`
	// TemplateMissing says the template isn't in this hlm's catalog; it's
	// added (as a custom template) when the server is installed.
	TemplateMissing bool `json:"templateMissing,omitempty"`
	// Settings lists the other settings that come along.
	Settings []string `json:"settings"`
	// Copies are the data folders a clone copies.
	Copies []Copy `json:"copies,omitempty"`
}

// Copy is a data folder a clone copies: Target in the container, from the
// original's host path to the clone's.
type Copy struct {
	Target string `json:"target"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// check refuses files hlm can't read.
func (e Export) check() error {
	switch {
	case e.HLMExport == 0:
		return errors.New("this isn't a server exported from hlm")
	case e.HLMExport > Version:
		return fmt.Errorf("this file is from a newer hlm (format %d; this hlm reads up to %d): update hlm first", e.HLMExport, Version)
	case e.Request.TemplateID == "":
		return errors.New("the file has no template")
	case e.Template != nil && e.Template.ID != e.Request.TemplateID:
		return errors.New("the file's template doesn't match its settings")
	}
	return nil
}

// template returns the template to install with, localized, and whether
// it has to be added to the catalog.
func (s *Service) template(e Export) (*catalog.App, bool, error) {
	if app, ok := s.opts.Manager.App(e.Request.TemplateID); ok {
		return app, false, nil
	}
	if e.Template == nil || len(e.Template.Config) == 0 && e.Template.Repository == "" {
		return nil, false, fmt.Errorf("template %q isn't in the catalog, and the file doesn't include it", e.Request.TemplateID)
	}
	return e.Template.Localize(s.opts.Manager.AppdataRoot()), true, nil
}

// localPaths puts the appdata root back into a request's paths.
func localPaths(req deploy.Request, root string) deploy.Request {
	root = strings.TrimRight(root, "/")
	req.Values = maps.Clone(req.Values)
	for k, v := range req.Values {
		if rest, ok := strings.CutPrefix(v, AppdataVar); ok {
			req.Values[k] = root + rest
		}
	}
	req.Extra = slices.Clone(req.Extra)
	for i, e := range req.Extra {
		if rest, ok := strings.CutPrefix(e.Value, AppdataVar); ok {
			req.Extra[i].Value = root + rest
		}
	}
	return req
}

// PreviewImport reads an export for the import form.
func (s *Service) PreviewImport(ctx context.Context, e Export) (Preview, error) {
	if err := e.check(); err != nil {
		return Preview{}, err
	}
	app, missing, err := s.template(e)
	if err != nil {
		return Preview{}, err
	}
	root := s.opts.Manager.AppdataRoot()
	req := localPaths(e.Request, root)
	p := Preview{App: app, Request: req, TemplateMissing: missing, Warnings: []string{}, Settings: e.Settings.Summary()}
	if p.Settings == nil {
		p.Settings = []string{}
	}
	names := itemNames(app, req)
	for _, id := range e.Omitted {
		p.Omitted = append(p.Omitted, names[id])
	}
	if len(p.Omitted) > 0 {
		p.Warnings = append(p.Warnings, "Passwords and keys weren't exported, so these are blank: "+strings.Join(p.Omitted, ", ")+
			". Fill them in before installing.")
	}
	if e.Settings.Console != nil && e.Settings.Console.Password == "" && e.Settings.Console.PasswordVar == "" &&
		(e.Settings.Console.Mode == console.ModeRCON || e.Settings.Console.Mode == console.ModeREST) {
		p.Warnings = append(p.Warnings, "The console's password wasn't exported: enter it in the console settings afterwards.")
	}
	if missing {
		p.Warnings = append(p.Warnings, fmt.Sprintf("The template %q isn't in this hlm's catalog: it's added from the file, as a custom template.", app.Name))
	}
	if req.IPAddress != "" {
		p.Warnings = append(p.Warnings, fmt.Sprintf("It has the fixed address %s on the %s network: make sure that's free here, or clear it.",
			req.IPAddress, req.Network))
	}
	used := s.pathsInUse(ctx)
	if items, err := deploy.Items(app, req); err == nil {
		for _, it := range items {
			if it.Type != catalog.TypePath || it.Disabled || it.Effective == "" {
				continue
			}
			if other, ok := used[path.Clean(it.Effective)]; ok {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s (%s) is already %s's: change it, or the two servers share it.",
					it.Name, it.Effective, other))
			} else if _, ok := under(it.Effective, strings.TrimRight(root, "/")); !ok {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s is outside the appdata folder, so it's kept as it was: %s.", it.Name, it.Effective))
			}
		}
	}
	if _, err := s.opts.Manager.Inspect(ctx, req.Name); err == nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("A server named %s already exists here: choose another name.", req.Name))
	}
	return p, nil
}

// pathsInUse maps the host folders servers here have as data folders to
// their servers.
func (s *Service) pathsInUse(ctx context.Context) map[string]string {
	out := map[string]string{}
	list, err := s.opts.Manager.List(ctx)
	if err != nil {
		return out
	}
	for _, srv := range list {
		info, err := s.opts.Manager.Inspect(ctx, srv.ID)
		if err != nil {
			continue
		}
		for _, m := range info.Mounts {
			if m.Type == "bind" {
				out[path.Clean(m.Source)] = srv.Name
			}
		}
	}
	return out
}

func itemNames(app *catalog.App, req deploy.Request) map[string]string {
	names := map[string]string{deploy.HiddenExtraParams: "Docker options (ExtraParams)", deploy.HiddenPostArgs: "post arguments"}
	if items, err := deploy.Items(app, req); err == nil {
		for _, it := range items {
			names[it.ID] = it.Name
		}
	}
	return names
}

// Import installs a server from an export, with req as the settings (the
// import form's, starting from PreviewImport's), and gives it the export's
// other settings.
func (s *Service) Import(ctx context.Context, e Export, req deploy.Request, emit func(manager.Event)) (manager.Server, error) {
	if err := e.check(); err != nil {
		return manager.Server{}, err
	}
	if _, missing, err := s.template(e); err != nil {
		return manager.Server{}, err
	} else if missing {
		if err := s.opts.Catalog.AddCustom(e.Template); err != nil {
			return manager.Server{}, fmt.Errorf("add the template: %w", err)
		}
		emit(manager.Event{Stage: "template", Message: "Added the template " + e.Template.Name + " to the catalog"})
	}
	req.TemplateID = e.Request.TemplateID
	if !ValidName.MatchString(req.Name) {
		return manager.Server{}, errors.New("invalid server name")
	}
	srv, err := s.opts.Manager.Install(ctx, req, emit)
	if err != nil {
		return srv, err
	}
	s.apply(ctx, srv.Name, e.Settings, emit)
	return srv, nil
}
