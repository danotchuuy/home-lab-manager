// Package backup copies chosen folders from game server containers to
// S3-compatible storage (such as SeaweedFS), on demand or on a schedule.
//
// Each backup is one tar.gz object, streamed straight from the container to
// the bucket, named <prefix>/<server>/<server>-<UTC time>.tar.gz. Entries are
// named by their container path, so extracting one at / inside the container
// (or under the matching data folders on the host) puts the files back.
//
// The bucket settings and each server's plan (folders, schedule, how many
// backups to keep) live in the encrypted secrets store.
package backup

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretSettings = "backup-s3"
	secretPlans    = "backup-plans"
	// secretSeries maps servers that moved to another Docker host to the
	// name their backups are kept under (the one they had before).
	secretSeries  = "backup-series"
	defaultPrefix = "hlm"
	// legacyDefaultPrefix was defaultPrefix before the short name changed
	// from gsh to hlm. Settings saved then without a prefix still use it.
	legacyDefaultPrefix = "gsh"
	timeLayout          = "20060102-150405"
)

// Settings are the stored bucket settings, credentials included. They only
// ever live in the encrypted secrets store.
type Settings struct {
	s3.Settings
	// Prefix is the folder in the bucket that backups go under.
	Prefix string `json:"prefix,omitempty"`
}

// PublicSettings are Settings without the secret key, for the UI.
type PublicSettings struct {
	Configured  bool   `json:"configured"`
	Endpoint    string `json:"endpoint,omitempty"`
	Region      string `json:"region,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	AccessKey   string `json:"accessKey,omitempty"`
	HasSecret   bool   `json:"hasSecret"`
	VirtualHost bool   `json:"virtualHost"`
	Prefix      string `json:"prefix,omitempty"`
	// CACert is the private CA trusted for the endpoint, if any. It isn't
	// secret, so it's shown in full, with a summary of each certificate.
	CACert  string        `json:"caCert,omitempty"`
	CACerts []s3.CertInfo `json:"caCerts,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// Plan is what to back up for one server, and when.
type Plan struct {
	Folders []string `json:"folders"`
	// IntervalHours is how often to back up automatically; 0 means only
	// when asked (unless At is set).
	IntervalHours int `json:"intervalHours"`
	// At backs up at a time of day instead of every IntervalHours.
	At *At `json:"at,omitempty"`
	// Since is when the schedule was last changed; At's first backup is
	// the first time after it.
	Since time.Time `json:"since,omitzero"`
	// Keep is how many backups to keep; older ones are deleted after each
	// successful backup. 0 keeps them all. Retention, if set, keeps by age
	// instead.
	Keep      int        `json:"keep"`
	Retention *Retention `json:"retention,omitempty"`
	// StopServer stops a running server for the backup, so its saves aren't
	// copied halfway through being written, and starts it again afterwards.
	StopServer bool `json:"stopServer"`
	// PreCommand and PostCommand are console commands (one per line) sent
	// before and after a running server is backed up, e.g. Minecraft's
	// save-off and save-all flush, then save-on.
	PreCommand  string `json:"preCommand,omitempty"`
	PostCommand string `json:"postCommand,omitempty"`
	// Exclude leaves files out; see files.Excluded.
	Exclude []string `json:"exclude,omitempty"`
	// Verify reads each backup back after it's stored: VerifyAuto (those
	// under 5 GB), VerifyAlways or VerifyOff.
	Verify string `json:"verify,omitempty"`
	// CopyTo also copies each backup to another backup storage.
	CopyTo *CopyPlan `json:"copyTo,omitempty"`
	// Checks are backups' latest verifications, by target and key.
	Checks map[string]Verification `json:"checks,omitempty"`
	Last   *Run                    `json:"last,omitempty"`
	// LastOK is the last backup that succeeded.
	LastOK *Run `json:"lastOk,omitempty"`
	// LastRestore is the last restore from a backup.
	LastRestore *Run `json:"lastRestore,omitempty"`
}

// Run is the outcome of one backup.
type Run struct {
	Trigger  string    `json:"trigger"` // manual, schedule or restore
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Key      string    `json:"key,omitempty"`
	Size     int64     `json:"size"`  // compressed, as stored
	Files    int       `json:"files"` // files backed up
	Bytes    int64     `json:"bytes"` // their size before compression
	Error    string    `json:"error,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
	// Before is, for a restore, the backup of the files it replaced.
	Before string `json:"before,omitempty"`
	// SHA256 is the stored backup's checksum.
	SHA256 string `json:"sha256,omitempty"`
	// Excluded counts the files and folders exclude patterns left out.
	Excluded int `json:"excluded,omitempty"`
	// Verify is how reading the backup back went, if it was.
	Verify *Verification `json:"verify,omitempty"`
	// Copy is how copying it to the plan's other storage went.
	Copy *CopyResult `json:"copy,omitempty"`
}

// Verify modes.
const (
	VerifyAuto   = ""
	VerifyAlways = "always"
	VerifyOff    = "off"
)

// verifyAutoLimit is the largest backup VerifyAuto reads back.
const verifyAutoLimit = 5 << 30

// CopyPlan copies a plan's backups to another backup storage, kept there
// by its own rules.
type CopyPlan struct {
	Target string `json:"target"`
	Keeping
}

// CopyResult is how copying a backup to another storage went.
type CopyResult struct {
	Target string `json:"target"`
	Key    string `json:"key,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (p Plan) keeping() Keeping { return Keeping{Keep: p.Keep, Retention: p.Retention} }

// Progress is a backup or restore in progress.
type Progress struct {
	Kind    string    `json:"kind"` // backup or restore
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
	// Stage is preparing, stopping, uploading, restoring, cleaning or
	// starting.
	Stage string `json:"stage"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	// Transferred is how much of the compressed backup has been uploaded or
	// downloaded so far.
	Transferred int64 `json:"transferred"`
	// Key is the backup being restored.
	Key string `json:"key,omitempty"`
}

// Servers is what the service needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	ArchiveFolders(ctx context.Context, id string, folders, exclude []string, tw *tar.Writer, progress func(files.ArchiveStats)) (files.ArchiveStats, error)
	RestoreArchive(ctx context.Context, id string, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
}

// Service runs backups.
type Service struct {
	secrets *secrets.Store
	servers Servers
	log     *slog.Logger

	plansMu sync.Mutex // serializes changes to the stored plans
	mu      sync.Mutex
	running map[string]*Progress // by server name, or configRunName
	config  *ConfigSource        // see UseConfig
	// notifier gets backups' and restores' results; see UseNotifier.
	notifier notify.Notifier
	// audit records scheduled backups; see UseAudit.
	audit audit.Recorder
	// console sends plans' pre and post commands; see UseConsole.
	console func(ctx context.Context, server, command string) error
	// location is hlm's time zone, for schedules and retention.
	location func() *time.Location
	// staleNotified is when each server was last reported as having no
	// recent backup, so it's checked once an hour.
	staleChecked time.Time
}

// UseConsole sets how plans' pre and post commands are sent.
func (s *Service) UseConsole(send func(ctx context.Context, server, command string) error) {
	s.console = send
}

// UseLocation sets hlm's time zone, for schedules at times of day and
// keeping one backup a day, week or month.
func (s *Service) UseLocation(loc func() *time.Location) { s.location = loc }

func (s *Service) loc() *time.Location {
	if s.location == nil {
		return time.Local
	}
	return s.location()
}

// UseAudit sets where scheduled backups are recorded (backups people start
// are recorded with their request).
func (s *Service) UseAudit(r audit.Recorder) { s.audit = r }

func (s *Service) auditScheduled(action, server string, run Run) {
	if s.audit == nil || run.Trigger != "schedule" {
		return
	}
	e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: action, Target: server, Server: server,
		Result: audit.ResultOK, Error: run.Error}
	if run.Error != "" {
		e.Result = audit.ResultError
	} else {
		e.Details = map[string]string{"key": run.Key}
	}
	s.audit.Record(e)
}

// UseNotifier sets where results are reported.
func (s *Service) UseNotifier(n notify.Notifier) { s.notifier = n }

// notifyBackup reports a backup's result. A failure is reported as a repeat
// only after a while, but the first failure after a success straight away.
// what is the server's name, or "" for hlm's configuration.
func (s *Service) notifyBackup(server string, run Run) {
	if s.notifier == nil {
		return
	}
	failKey := notify.DedupeKey(notify.KindBackupFailed, cmp.Or(server, configRunName))
	what, link := server, "/servers/"+server+"?tab=backups"
	if server == "" {
		what, link = "hlm's configuration", "/system#config-backup"
	}
	fields := map[string]string{"Trigger": run.Trigger}
	if run.Error != "" {
		s.notifier.Notify(notify.Event{
			Kind: notify.KindBackupFailed, Title: "Backup of " + what + " failed", Message: run.Error,
			Server: server, Link: link, Key: failKey, Fields: fields,
		})
		return
	}
	s.notifier.Clear(failKey)
	if server != "" {
		s.notifier.Clear(notify.DedupeKey(notify.KindBackupStale, server))
	}
	fields["Size"] = humanBytes(run.Size)
	fields["Files"] = strconv.Itoa(run.Files)
	msg := "Saved as " + run.Key + "."
	if len(run.Warnings) > 0 {
		msg += "\n\nWarnings:\n- " + strings.Join(run.Warnings[:min(len(run.Warnings), 10)], "\n- ")
	}
	s.notifier.Notify(notify.Event{
		Kind: notify.KindBackupSucceeded, Title: "Backed up " + what, Message: msg, Server: server, Link: link, Fields: fields,
	})
}

// notifyVerifyFailed reports a backup that couldn't be read back.
func (s *Service) notifyVerifyFailed(server, key string, v Verification) {
	if s.notifier == nil {
		return
	}
	s.notifier.Notify(notify.Event{
		Kind: notify.KindBackupVerifyFailed, Title: "A backup of " + server + " is damaged",
		Message: fmt.Sprintf("Reading %s back failed: %s. Back up again, and check the backup storage.", key, v.Error),
		Server:  server, Link: "/servers/" + server + "?tab=backups", Fields: map[string]string{"Backup": key},
		Key: notify.DedupeKey(notify.KindBackupVerifyFailed, server) + "/" + key,
	})
}

// staleCheckEvery is how often servers are checked for backups that are
// overdue.
const staleCheckEvery = time.Hour

// staleAfter is how long after its last successful backup a scheduled
// plan counts as having none recently: twice its schedule's gap, and at
// least a day and a half.
func (p Plan) staleAfter(loc *time.Location, now time.Time) time.Duration {
	gap := time.Duration(p.IntervalHours) * time.Hour
	if p.At != nil {
		// The longest gap between its next runs (weekday schedules have
		// longer ones now and then).
		t := p.At.next(now, loc)
		for i := 0; i < 8 && !t.IsZero(); i++ {
			n := p.At.next(t, loc)
			if !n.IsZero() && n.Sub(t) > gap {
				gap = n.Sub(t)
			}
			t = n
		}
	}
	if gap <= 0 {
		return 0
	}
	return max(2*gap, 36*time.Hour)
}

// checkStale reports scheduled plans whose last successful backup is
// overdue.
func (s *Service) checkStale(plans map[string]Plan, now time.Time) {
	if s.notifier == nil || now.Sub(s.staleChecked) < staleCheckEvery {
		return
	}
	s.staleChecked = now
	for name, p := range plans {
		after := p.staleAfter(s.loc(), now)
		if after == 0 || len(p.Folders) == 0 {
			continue
		}
		since := p.Since
		if p.LastOK != nil {
			since = p.LastOK.Started
		}
		if since.IsZero() || now.Sub(since) < after {
			continue
		}
		what := "No backup of " + name + " has worked since " + since.In(s.loc()).Format("Mon 2 Jan 15:04")
		if p.LastOK == nil {
			what = "No backup of " + name + " has worked since its schedule was set on " + since.In(s.loc()).Format("Mon 2 Jan 15:04")
		}
		msg := what + "."
		if p.Last != nil && p.Last.Error != "" {
			msg += "\n\nThe last one failed: " + p.Last.Error
		}
		s.notifier.Notify(notify.Event{
			Kind: notify.KindBackupStale, Title: "No recent backup of " + name, Message: msg, Server: name,
			Link: "/servers/" + name + "?tab=backups",
		})
	}
}

// notifyRestore reports a restore's result.
func (s *Service) notifyRestore(server string, run Run) {
	if s.notifier == nil {
		return
	}
	e := notify.Event{
		Kind: notify.KindRestoreFinished, Title: "Restored " + server, Server: server,
		Link: "/servers/" + server + "?tab=backups", Fields: map[string]string{"Files": strconv.Itoa(run.Files)},
		Message: "Restored " + strconv.Itoa(run.Files) + " files.",
	}
	if run.Error != "" {
		e.Kind, e.Title, e.Message = notify.KindRestoreFailed, "Restoring "+server+" failed", run.Error
	}
	s.notifier.Notify(e)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

// New creates the service.
func New(store *secrets.Store, servers Servers, log *slog.Logger) *Service {
	return &Service{secrets: store, servers: servers, log: log, running: map[string]*Progress{}}
}

// ErrNotConfigured is returned when no bucket has been set up.
var ErrNotConfigured = errors.New("backup storage isn't set up yet; add it under System → Backup storage")

// ErrRunning is returned when a server's backup or restore is already
// running.
var ErrRunning = errors.New("already running")

// Configured reports whether backup storage has been set up.
func (s *Service) Configured() bool {
	ts, err := s.targets()
	return err == nil && len(ts) > 0
}

// PublicSettings returns the primary storage's settings, without the
// secret key.
func (s *Service) PublicSettings() PublicSettings {
	t, err := s.target("")
	if errors.Is(err, ErrNotConfigured) {
		return PublicSettings{}
	}
	if err != nil {
		return PublicSettings{Error: err.Error()}
	}
	return publicSettings(t.Settings)
}

func publicSettings(c Settings) PublicSettings {
	return PublicSettings{
		Configured: true, Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, AccessKey: c.AccessKey,
		HasSecret: c.SecretKey != "", VirtualHost: c.VirtualHost, Prefix: c.prefix(),
		CACert: c.CACert, CACerts: s3.DescribeCACert(c.CACert),
	}
}

// prefix is the folder backups go under. Settings are saved with one (see
// merge), so an empty one was saved before the rename from gsh to hlm.
func (c Settings) prefix() string {
	if c.Prefix == "" {
		return legacyDefaultPrefix
	}
	return c.Prefix
}

// mergeSettings keeps the saved secret key when in leaves it empty for the
// same endpoint and access key, and tidies the rest.
func mergeSettings(in, saved Settings) (Settings, error) {
	in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	in.AccessKey = strings.TrimSpace(in.AccessKey)
	if in.SecretKey == "" && in.Endpoint == saved.Endpoint && in.AccessKey == saved.AccessKey {
		in.SecretKey = saved.SecretKey
	}
	in.Region = strings.TrimSpace(in.Region)
	if in.CACert = strings.TrimSpace(in.CACert); in.CACert != "" {
		in.CACert += "\n"
	}
	if in.Prefix = strings.Trim(strings.TrimSpace(in.Prefix), "/"); in.Prefix == "" {
		in.Prefix = defaultPrefix
	}
	if strings.Contains(in.Prefix, "//") {
		return in, errors.New("the folder in the bucket can't contain empty parts (//)")
	}
	return in, nil
}

// merge is mergeSettings with the primary's saved settings.
func (s *Service) merge(in Settings) (Settings, error) {
	saved, err := s.target("")
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return in, err
	}
	return mergeSettings(in, saved.Settings)
}

// Test checks the given (possibly unsaved) settings by listing the bucket
// and writing, then deleting, a small file.
func (s *Service) Test(ctx context.Context, in Settings) (int, error) {
	c, err := s.merge(in)
	if err != nil {
		return 0, err
	}
	return test(ctx, c)
}

func test(ctx context.Context, c Settings) (int, error) {
	cl, err := s3.New(c.Settings)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	objs, err := cl.List(ctx, c.prefix()+"/")
	if err != nil {
		return 0, fmt.Errorf("list the bucket: %w", err)
	}
	key := c.prefix() + "/.hlm-write-test"
	if err := cl.Put(ctx, key, []byte("hlm can write here\n"), "text/plain"); err != nil {
		return 0, fmt.Errorf("write a test file: %w", err)
	}
	if err := cl.Delete(ctx, key); err != nil {
		return 0, fmt.Errorf("delete the test file: %w", err)
	}
	n := 0
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".tar.gz") {
			n++
		}
	}
	return n, nil
}

// Save stores the primary storage's settings after checking that they
// work.
func (s *Service) Save(ctx context.Context, in Settings) (PublicSettings, error) {
	t, err := s.target("")
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return PublicSettings{}, err
	}
	t.Settings = in
	if t.Name == "" {
		t.Name = "Primary"
	}
	if _, err := s.SaveTarget(ctx, t); err != nil {
		return PublicSettings{}, err
	}
	return s.PublicSettings(), nil
}

// Delete forgets the primary storage's settings, when it's the only one.
// Servers' plans and the backups in the bucket are kept.
func (s *Service) Delete() error {
	t, err := s.target("")
	if errors.Is(err, ErrNotConfigured) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.DeleteTarget(t.ID)
}

func (s *Service) plans() (map[string]Plan, error) {
	plans := map[string]Plan{}
	_, err := s.secrets.Get(secretPlans, &plans)
	return plans, err
}

// seriesName is the name a server's backups are kept under: its own, or,
// for a server that moved to another Docker host, the one it had before.
func (s *Service) seriesName(name string) string {
	var m map[string]string
	if _, err := s.secrets.Get(secretSeries, &m); err == nil && m[name] != "" {
		return m[name]
	}
	return name
}

// RenameServer moves a server's backup plan to its new name, when it moves
// to another Docker host. Its backups stay where they are, under the name
// they were taken under, and new ones go with them, so the history carries
// on without copying anything.
func (s *Service) RenameServer(from, to string) error {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	if _, err := s.secrets.MoveKey(secretPlans, from, to); err != nil {
		return err
	}
	m := map[string]string{}
	if _, err := s.secrets.Get(secretSeries, &m); err != nil {
		return err
	}
	if m == nil {
		m = map[string]string{}
	}
	series := m[from]
	if series == "" {
		series = from
	}
	delete(m, from)
	if series == to {
		delete(m, to)
	} else {
		m[to] = series
	}
	return s.secrets.Set(secretSeries, m)
}

// Plans returns every server's backup plan and last results, by name.
func (s *Service) Plans() (map[string]Plan, error) { return s.plans() }

// updatePlan changes one server's stored plan.
func (s *Service) updatePlan(name string, fn func(*Plan)) error {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	plans, err := s.plans()
	if err != nil {
		return err
	}
	p := plans[name]
	fn(&p)
	plans[name] = p
	return s.secrets.Set(secretPlans, plans)
}

// serverName resolves a container ID or name to the server's name, which
// plans and backups are kept by (containers are recreated on updates).
func (s *Service) serverName(ctx context.Context, id string) (docker.ContainerJSON, string, error) {
	info, err := s.servers.Inspect(ctx, id)
	if err != nil {
		return info, "", err
	}
	return info, strings.TrimPrefix(info.Name, "/"), nil
}

// Overview is everything the Backups tab shows for a server.
type Overview struct {
	Configured bool        `json:"configured"`
	Plan       Plan        `json:"plan"`
	Running    *Progress   `json:"running,omitempty"`
	Backups    []s3.Object `json:"backups"`
	// Error is set when the bucket couldn't be listed.
	Error string `json:"error,omitempty"`
	// Target is the primary storage's ID and name.
	Target TargetRef `json:"target"`
	// Copies are the backups in the storage the plan copies to.
	Copies *CopiesOverview `json:"copies,omitempty"`
	// Targets are the other storage a plan can copy to.
	Targets []TargetRef `json:"targets"`
}

// TargetRef names a backup storage.
type TargetRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Encrypt bool   `json:"encrypt,omitempty"`
}

// CopiesOverview is a plan's copies in another storage.
type CopiesOverview struct {
	Target  TargetRef   `json:"target"`
	Backups []s3.Object `json:"backups"`
	Error   string      `json:"error,omitempty"`
}

// Overview returns a server's plan, its running backup and the backups in
// the bucket, newest first.
func (s *Service) Overview(ctx context.Context, id string) (Overview, error) {
	_, name, err := s.serverName(ctx, id)
	if err != nil {
		return Overview{}, err
	}
	plans, err := s.plans()
	if err != nil {
		return Overview{}, err
	}
	o := Overview{Plan: plans[name], Backups: []s3.Object{}}
	if o.Plan.Folders == nil {
		o.Plan.Folders = []string{}
	}
	s.mu.Lock()
	if p := s.running[name]; p != nil {
		cp := *p
		o.Running = &cp
	}
	s.mu.Unlock()

	o.Targets = []TargetRef{}
	ts, err := s.targets()
	if err != nil || len(ts) == 0 {
		if err != nil {
			o.Error = err.Error()
		}
		return o, nil
	}
	o.Configured = true
	o.Target = TargetRef{ts[0].ID, ts[0].Name, ts[0].Encrypt}
	for _, t := range ts[1:] {
		o.Targets = append(o.Targets, TargetRef{t.ID, t.Name, t.Encrypt})
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	list := func(id string) ([]s3.Object, error) {
		st, err := s.store(id)
		if err != nil {
			return nil, err
		}
		objs, err := serverSeries(st.t.Settings, s.seriesName(name)).list(ctx, st.cl)
		return newestFirst(objs), err
	}
	if o.Backups, err = list(""); err != nil {
		o.Error = err.Error()
	}
	if o.Backups == nil {
		o.Backups = []s3.Object{}
	}
	if cp := o.Plan.CopyTo; cp != nil {
		c := &CopiesOverview{Target: TargetRef{ID: cp.Target, Name: cp.Target}, Backups: []s3.Object{}}
		for _, t := range ts {
			if t.ID == cp.Target {
				c.Target = TargetRef{t.ID, t.Name, t.Encrypt}
			}
		}
		if objs, err := list(cp.Target); err != nil {
			c.Error = err.Error()
		} else if objs != nil {
			c.Backups = objs
		}
		o.Copies = c
	}
	return o, nil
}

// series is a set of backups: those in one folder of the bucket whose names
// start the same way, then end in their UTC time.
type series struct {
	dir  string // ends in /
	file string // ends in -
	// legacy is where the same backups went before the short name changed
	// from gsh to hlm, if elsewhere. They're listed (and pruned) too.
	legacy *series
}

// serverSeries is a server's backups: <prefix>/<server>/<server>-<time>.tar.gz.
func serverSeries(c Settings, name string) series {
	return series{dir: c.prefix() + "/" + name + "/", file: name + "-"}
}

// newKey names a new backup taken at t.
func (r series) newKey(t time.Time) string {
	return r.dir + r.file + t.UTC().Format(timeLayout) + ".tar.gz"
}

// validKey matches the end of a key newKey made.
var validKey = regexp.MustCompile(`^\d{8}-\d{6}\.tar\.gz$`)

// has reports whether key is one of the series' backups.
func (r series) has(key string) bool {
	if r.legacy != nil && r.legacy.has(key) {
		return true
	}
	rest, ok := strings.CutPrefix(key, r.dir+r.file)
	return ok && validKey.MatchString(rest)
}

// list returns the series' backups, oldest first.
func (r series) list(ctx context.Context, cl *s3.Client) ([]s3.Object, error) {
	objs, err := cl.List(ctx, r.dir)
	if err != nil {
		return nil, err
	}
	var out []s3.Object
	for _, o := range objs {
		if rest, ok := strings.CutPrefix(o.Key, r.dir+r.file); ok && validKey.MatchString(rest) {
			out = append(out, o)
		}
	}
	if r.legacy != nil {
		old, err := r.legacy.list(ctx, cl)
		if err != nil {
			return nil, err
		}
		out = append(out, old...)
	}
	// Keys end in a sortable UTC time.
	at := func(key string) string { return key[len(key)-len(timeLayout+".tar.gz"):] }
	sort.Slice(out, func(i, j int) bool { return at(out[i].Key) < at(out[j].Key) })
	return out, nil
}

// newestFirst reverses a list from series.list, never returning nil.
func newestFirst(objs []s3.Object) []s3.Object {
	out := make([]s3.Object, 0, len(objs))
	for i := len(objs) - 1; i >= 0; i-- {
		out = append(out, objs[i])
	}
	return out
}

// SetPlan saves a server's plan. Its history is kept.
func (s *Service) SetPlan(ctx context.Context, id string, in Plan) (Plan, error) {
	_, name, err := s.serverName(ctx, id)
	if err != nil {
		return Plan{}, err
	}
	folders, err := files.NormalizeFolders(in.Folders)
	if err != nil {
		return Plan{}, err
	}
	if in.IntervalHours < 0 || in.IntervalHours > 24*365 {
		return Plan{}, errors.New("the interval must be between 0 (off) and 8760 hours")
	}
	if in.At != nil {
		at, err := in.At.check()
		if err != nil {
			return Plan{}, err
		}
		in.At, in.IntervalHours = &at, 0
	}
	if err := in.keeping().check(); err != nil {
		return Plan{}, err
	}
	if (in.IntervalHours > 0 || in.At != nil) && len(folders) == 0 {
		return Plan{}, errors.New("choose at least one folder before turning on scheduled backups")
	}
	exclude, err := files.CheckExclude(in.Exclude)
	if err != nil {
		return Plan{}, err
	}
	switch in.Verify {
	case VerifyAuto, VerifyAlways, VerifyOff:
	default:
		return Plan{}, fmt.Errorf("unknown verify setting %q", in.Verify)
	}
	pre, post := cleanCommands(in.PreCommand), cleanCommands(in.PostCommand)
	if (pre != "" || post != "") && s.console == nil {
		return Plan{}, errors.New("commands before and after a backup need the console")
	}
	if in.CopyTo != nil {
		if in.CopyTo.Target == "" {
			in.CopyTo = nil
		} else {
			t, err := s.target(in.CopyTo.Target)
			if err != nil {
				return Plan{}, err
			}
			if primary, _ := s.target(""); primary.ID == t.ID {
				return Plan{}, errors.New("backups already go to the primary storage: choose another to copy them to")
			}
			if err := in.CopyTo.check(); err != nil {
				return Plan{}, fmt.Errorf("copies: %w", err)
			}
		}
	}
	var out Plan
	err = s.updatePlan(name, func(p *Plan) {
		if p.IntervalHours != in.IntervalHours || !sameAt(p.At, in.At) {
			p.Since = time.Now()
		}
		p.Folders, p.IntervalHours, p.At, p.Keep, p.Retention, p.StopServer = folders, in.IntervalHours, in.At, in.Keep, in.Retention, in.StopServer
		p.PreCommand, p.PostCommand, p.Exclude, p.Verify, p.CopyTo = pre, post, exclude, in.Verify, in.CopyTo
		out = *p
	})
	if out.Folders == nil {
		out.Folders = []string{}
	}
	return out, err
}

func sameAt(a, b *At) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Time == b.Time && slices.Equal(a.Weekdays, b.Weekdays)
}

// cleanCommands trims each line of console commands and drops empty ones.
func cleanCommands(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// Start begins a backup of a server in the background.
func (s *Service) Start(ctx context.Context, id string) error {
	_, name, err := s.serverName(ctx, id)
	if err != nil {
		return err
	}
	return s.start(name, "manual")
}

func (s *Service) start(name, trigger string) error {
	plan, prog, err := s.begin(name, trigger)
	if err != nil {
		return err
	}
	go s.finish(name, plan, prog)
	return nil
}

// begin checks a server's backup can run and claims it.
func (s *Service) begin(name, trigger string) (Plan, *Progress, error) {
	if _, _, err := s.client(); err != nil {
		return Plan{}, nil, err
	}
	plans, err := s.plans()
	if err != nil {
		return Plan{}, nil, err
	}
	plan := plans[name]
	if len(plan.Folders) == 0 {
		return Plan{}, nil, errors.New("choose the folders to back up first")
	}
	prog, err := s.claim(name, "backup", trigger)
	if err != nil {
		return Plan{}, nil, err
	}
	return plan, prog, nil
}

// finish runs a claimed backup, records and reports its result.
func (s *Service) finish(name string, plan Plan, prog *Progress) Run {
	run := s.run(context.Background(), name, plan, prog)
	s.release(name)
	if run.Error != "" {
		s.log.Warn("backup failed", "server", name, "err", run.Error)
	} else {
		s.log.Info("backup finished", "server", name, "key", run.Key, "files", run.Files, "size", run.Size)
	}
	if err := s.updatePlan(name, func(p *Plan) {
		p.Last = &run
		if run.Error == "" {
			p.LastOK = &run
		}
	}); err != nil {
		s.log.Warn("record backup result", "server", name, "err", err)
	}
	s.notifyBackup(name, run)
	s.auditScheduled("servers.backups.scheduled", name, run)
	return run
}

// BackupNow backs up a server (by name or ID) and waits for it to finish.
// The backup isn't cut short if ctx ends.
func (s *Service) BackupNow(ctx context.Context, id, trigger string) (Run, error) {
	_, name, err := s.serverName(ctx, id)
	if err != nil {
		return Run{}, err
	}
	plan, prog, err := s.begin(name, trigger)
	if err != nil {
		return Run{}, err
	}
	run := s.finish(name, plan, prog)
	if run.Error != "" {
		return run, errors.New(run.Error)
	}
	return run, nil
}

// HasPlan reports whether backups are set up and the server called name
// has folders chosen to back up.
func (s *Service) HasPlan(name string) bool {
	if !s.Configured() {
		return false
	}
	plans, err := s.plans()
	return err == nil && len(plans[name].Folders) > 0
}

// Busy reports whether a backup or restore of the server called name is
// running.
func (s *Service) Busy(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[name] != nil
}

// claim marks a backup or restore of a server as running; only one runs
// at a time.
func (s *Service) claim(name, kind, trigger string) (*Progress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.running[name]; p != nil {
		return nil, fmt.Errorf("a %s of this server is %w", p.Kind, ErrRunning)
	}
	prog := &Progress{Kind: kind, Trigger: trigger, Started: time.Now(), Stage: "preparing"}
	s.running[name] = prog
	return prog, nil
}

func (s *Service) release(name string) {
	s.mu.Lock()
	delete(s.running, name)
	s.mu.Unlock()
}

func (s *Service) setStage(prog *Progress, stage string) {
	s.mu.Lock()
	prog.Stage = stage
	s.mu.Unlock()
}

// run performs one backup and reports how it went.
func (s *Service) run(ctx context.Context, name string, plan Plan, prog *Progress) (run Run) {
	run = Run{Trigger: prog.Trigger, Started: prog.Started}
	fail := func(err error) Run {
		run.Finished, run.Error = time.Now(), err.Error()
		return run
	}
	st, err := s.store("")
	if err != nil {
		return fail(err)
	}
	if err := s.capture(ctx, st, name, plan, prog, &run); err != nil {
		return fail(err)
	}
	if plan.verifies(run.Size) {
		s.setStage(prog, "verifying")
		v := st.verify(ctx, run.Key, run.SHA256, run.Size, run.Files)
		run.Verify = &v
		s.recordCheck(name, st.t.ID, run.Key, v)
		if !v.OK {
			s.notifyVerifyFailed(name, run.Key, v)
			return fail(fmt.Errorf("the backup was stored, but reading it back failed: %s", v.Error))
		}
	}
	if plan.CopyTo != nil {
		s.setStage(prog, "copying")
		run.Copy = s.copyTo(ctx, st, name, run.Key, *plan.CopyTo)
		if run.Copy.Error != "" {
			run.Warnings = append(run.Warnings, "copying it to "+run.Copy.Target+" failed: "+run.Copy.Error)
		}
	}
	if plan.keeping().prunes() {
		s.setStage(prog, "cleaning")
		if err := prune(ctx, st, serverSeries(st.t.Settings, s.seriesName(name)), plan.keeping(), s.loc()); err != nil {
			run.Warnings = append(run.Warnings, "couldn't delete old backups: "+err.Error())
		}
	}
	run.Finished = time.Now()
	return run
}

// verifies reports whether a backup of this size is read back.
func (p Plan) verifies(size int64) bool {
	switch p.Verify {
	case VerifyOff:
		return false
	case VerifyAlways:
		return true
	}
	return size < verifyAutoLimit
}

// capture stores the backup itself: the pre commands, stopping the server,
// the upload, starting it again and the post commands.
func (s *Service) capture(ctx context.Context, st *store, name string, plan Plan, prog *Progress, run *Run) error {
	if plan.PreCommand != "" || plan.PostCommand != "" {
		if info, err := s.servers.Inspect(ctx, name); err == nil && info.State.Running && s.console != nil {
			s.setStage(prog, "commands")
			s.commands(ctx, name, plan.PreCommand, "before", run)
			defer s.commands(context.WithoutCancel(ctx), name, plan.PostCommand, "after", run)
		}
	}
	if plan.StopServer {
		resume, err := s.pause(ctx, name, prog, run)
		if err != nil {
			return err
		}
		defer resume()
	}
	return s.upload(ctx, st, name, plan.Folders, plan.Exclude, prog, run)
}

// commands sends console commands, one per line, adding a warning for each
// that fails: the backup goes ahead anyway.
func (s *Service) commands(ctx context.Context, name, lines, when string, run *Run) {
	for _, c := range strings.Split(lines, "\n") {
		if c == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.console(cctx, name, c)
		cancel()
		if err != nil {
			run.Warnings = append(run.Warnings, fmt.Sprintf("the command %q %s the backup failed: %v", c, when, err))
		}
	}
}

// copyTo copies a stored backup to another storage, and prunes the copies
// there by the copy's own rules.
func (s *Service) copyTo(ctx context.Context, from *store, name, key string, cp CopyPlan) *CopyResult {
	res := &CopyResult{Target: cp.Target}
	to, err := s.store(cp.Target)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Target = to.t.Name
	t, ok := keyTime(key)
	if !ok {
		res.Error = "unexpected key " + key
		return res
	}
	r := serverSeries(to.t.Settings, s.seriesName(name))
	dst := r.newKey(t)
	rc, _, err := from.open(ctx, key)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer rc.Close()
	if _, _, err := to.put(ctx, dst, rc, map[string]string{"Hlm-Server": name}); err != nil {
		res.Error = err.Error()
		return res
	}
	res.Key = dst
	if cp.prunes() {
		if err := prune(ctx, to, r, cp.Keeping, s.loc()); err != nil {
			res.Error = "copied, but deleting old copies failed: " + err.Error()
		}
	}
	return res
}

// recordCheck keeps a backup's latest verification, for the Backups tab.
func (s *Service) recordCheck(name, target, key string, v Verification) {
	if err := s.updatePlan(name, func(p *Plan) {
		if p.Checks == nil {
			p.Checks = map[string]Verification{}
		}
		p.Checks[target+":"+key] = v
		// Keep the newest checks.
		for len(p.Checks) > 200 {
			oldest := ""
			for k, c := range p.Checks {
				if oldest == "" || c.At.Before(p.Checks[oldest].At) {
					oldest = k
				}
			}
			delete(p.Checks, oldest)
		}
	}); err != nil {
		s.log.Warn("record verification", "server", name, "err", err)
	}
}

// pause stops the server if it is running, and returns a function that
// starts it again, adding a warning to run if that fails.
func (s *Service) pause(ctx context.Context, name string, prog *Progress, run *Run) (func(), error) {
	info, err := s.servers.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	if !info.State.Running {
		return func() {}, nil
	}
	s.setStage(prog, "stopping")
	s.log.Info("stopping server", "server", name, "for", prog.Kind)
	if err := s.servers.Stop(ctx, name); err != nil {
		return nil, fmt.Errorf("stop the server: %w", err)
	}
	return func() {
		s.setStage(prog, "starting")
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := s.servers.Start(sctx, name); err != nil {
			run.Warnings = append(run.Warnings, "the server couldn't be started again afterwards: "+err.Error())
		}
	}, nil
}

// upload archives folders into a new backup, filling in run's key, size,
// checksum, counts and warnings.
func (s *Service) upload(ctx context.Context, st *store, name string, folders, exclude []string, prog *Progress, run *Run) error {
	s.setStage(prog, "uploading")
	key := serverSeries(st.t.Settings, s.seriesName(name)).newKey(time.Now())
	pr, pw := io.Pipe()
	type result struct {
		stats files.ArchiveStats
		err   error
	}
	done := make(chan result, 1)
	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		st, err := s.servers.ArchiveFolders(ctx, name, folders, exclude, tw, func(st files.ArchiveStats) {
			s.mu.Lock()
			prog.Files, prog.Bytes = st.Files, st.Bytes
			s.mu.Unlock()
		})
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = gz.Close()
		}
		pw.CloseWithError(err) // nil closes it normally
		done <- result{st, err}
	}()

	counted := &countingReader{r: pr, add: func(n int) {
		s.mu.Lock()
		prog.Transferred += int64(n)
		s.mu.Unlock()
	}}
	size, sum, err := st.put(ctx, key, counted, map[string]string{"Hlm-Server": name})
	// Unblock the archiver if the upload stopped reading.
	pr.CloseWithError(errors.New("upload stopped"))
	res := <-done
	run.Warnings = append(run.Warnings, res.stats.Warnings...)
	run.Files, run.Bytes, run.Excluded = res.stats.Files, res.stats.Bytes, res.stats.Excluded
	var noSum *sumError
	if errors.As(err, &noSum) {
		run.Warnings = append(run.Warnings, err.Error())
		err = nil
	}
	// A failed archive also fails the upload, with the same error; a failed
	// upload makes the archive fail only because nothing reads it anymore.
	if err != nil {
		return err
	}
	if res.err != nil {
		return res.err
	}
	run.Key, run.Size, run.SHA256 = key, size, sum
	return nil
}

// Restore puts the files from one of a server's backups back in its
// container, in the background. The server is stopped meanwhile and started
// again afterwards if it was running. With backupFirst, the server's chosen
// folders are backed up first (without deleting old backups), so the
// restore can be undone; if that fails, nothing is restored.
func (s *Service) Restore(ctx context.Context, id, key, target string, backupFirst bool) error {
	src, name, err := s.backupKey(ctx, id, key, target)
	if err != nil {
		return err
	}
	plans, err := s.plans()
	if err != nil {
		return err
	}
	plan := plans[name]
	if backupFirst && len(plan.Folders) == 0 {
		return errors.New("no folders are chosen, so the current files can't be backed up first")
	}
	prog, err := s.claim(name, "restore", "restore")
	if err != nil {
		return err
	}
	prog.Key = key
	go func() {
		run := s.restore(context.Background(), src, name, key, plan, backupFirst, prog)
		s.release(name)
		if run.Error != "" {
			s.log.Warn("restore failed", "server", name, "key", key, "err", run.Error)
		} else {
			s.log.Info("restore finished", "server", name, "key", key, "files", run.Files)
		}
		if err := s.updatePlan(name, func(p *Plan) { p.LastRestore = &run }); err != nil {
			s.log.Warn("record restore result", "server", name, "err", err)
		}
		s.notifyRestore(name, run)
	}()
	return nil
}

// restore performs a restore; see Restore. The result is named for the
// same reason as run's.
func (s *Service) restore(ctx context.Context, src *store, name, key string, plan Plan, backupFirst bool, prog *Progress) (run Run) {
	run = Run{Trigger: "restore", Started: prog.Started}
	fail := func(err error) Run {
		run.Finished, run.Error = time.Now(), err.Error()
		return run
	}
	primary, err := s.store("")
	if err != nil {
		return fail(err)
	}
	resume, err := s.pause(ctx, name, prog, &run)
	if err != nil {
		return fail(err)
	}
	defer resume()

	if backupFirst {
		var before Run
		if err := s.upload(ctx, primary, name, plan.Folders, plan.Exclude, prog, &before); err != nil {
			return fail(fmt.Errorf("back up the current files first: %w", err))
		}
		run.Before = before.Key
		s.mu.Lock()
		prog.Files, prog.Bytes, prog.Transferred = 0, 0, 0
		s.mu.Unlock()
	}

	s.setStage(prog, "restoring")
	rc, _, err := src.cl.Get(ctx, key)
	if err != nil {
		return fail(err)
	}
	defer rc.Close()
	counted := &countingReader{r: rc, add: func(n int) {
		s.mu.Lock()
		prog.Transferred += int64(n)
		s.mu.Unlock()
	}}
	plain, err := src.plain(counted)
	if err != nil {
		return fail(err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return fail(fmt.Errorf("read the backup: %w", err))
	}
	st, err := s.servers.RestoreArchive(ctx, name, gz, func(st files.ArchiveStats) {
		s.mu.Lock()
		prog.Files, prog.Bytes = st.Files, st.Bytes
		s.mu.Unlock()
	})
	run.Key, run.Files, run.Bytes, run.Warnings = key, st.Files, st.Bytes, append(run.Warnings, st.Warnings...)
	if err != nil {
		return fail(err)
	}
	run.Finished = time.Now()
	return run
}

// IsBackupOf reports whether key is one of server name's backups.
func (s *Service) IsBackupOf(name, key string) bool {
	t, err := s.target("")
	return err == nil && serverSeries(t.Settings, s.seriesName(name)).has(key)
}

// RestoreInto extracts one of server from's backups into another server's
// container (a new copy of it, say), and waits for it to finish. The
// target should be stopped. progress, if not nil, is called as files are
// written.
func (s *Service) RestoreInto(ctx context.Context, from, key, toID string, progress func(files.ArchiveStats)) (Run, error) {
	src, err := s.store("")
	if err != nil {
		return Run{}, err
	}
	if !serverSeries(src.t.Settings, s.seriesName(from)).has(key) {
		return Run{}, ErrBadKey
	}
	_, to, err := s.serverName(ctx, toID)
	if err != nil {
		return Run{}, err
	}
	prog, err := s.claim(to, "restore", "restore")
	if err != nil {
		return Run{}, err
	}
	defer s.release(to)
	prog.Key = key
	run := Run{Trigger: "restore", Started: prog.Started, Key: key}
	s.setStage(prog, "restoring")
	rc, _, err := src.open(ctx, key)
	if err != nil {
		return run, err
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return run, fmt.Errorf("read the backup: %w", err)
	}
	st, err := s.servers.RestoreArchive(ctx, toID, gz, func(st files.ArchiveStats) {
		s.mu.Lock()
		prog.Files, prog.Bytes = st.Files, st.Bytes
		s.mu.Unlock()
		if progress != nil {
			progress(st)
		}
	})
	run.Files, run.Bytes, run.Warnings, run.Finished = st.Files, st.Bytes, st.Warnings, time.Now()
	if err != nil {
		run.Error = err.Error()
	}
	return run, err
}

// prune deletes the backups of a series that k doesn't keep.
func prune(ctx context.Context, st *store, r series, k Keeping, loc *time.Location) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	objs, err := r.list(ctx, st.cl)
	if err != nil {
		return err
	}
	objs = newestFirst(objs)
	times := make([]time.Time, len(objs))
	for i, o := range objs {
		times[i], _ = keyTime(o.Key)
	}
	keep := k.keepSet(times, loc)
	for i, o := range objs {
		if !keep[i] {
			if err := st.remove(ctx, o.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

type countingReader struct {
	r   io.Reader
	add func(int)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.add(n)
	return n, err
}

// ErrBadKey is returned for an object that isn't one of the backups asked
// about (a server's, or hlm's configuration).
var ErrBadKey = errors.New("that isn't one of these backups")

// backupKey checks that key is one of the server's backups in a target
// ("" is the primary), and returns the target's store and the server's
// name.
func (s *Service) backupKey(ctx context.Context, id, key, target string) (*store, string, error) {
	_, name, err := s.serverName(ctx, id)
	if err != nil {
		return nil, "", err
	}
	st, err := s.store(target)
	if err != nil {
		return nil, "", err
	}
	if !serverSeries(st.t.Settings, s.seriesName(name)).has(key) {
		return nil, "", ErrBadKey
	}
	return st, name, nil
}

// Download opens one of a server's backups, decrypted. size is -1 when
// it's decrypted (and so isn't known).
func (s *Service) Download(ctx context.Context, id, key, target string) (io.ReadCloser, int64, error) {
	st, _, err := s.backupKey(ctx, id, key, target)
	if err != nil {
		return nil, 0, err
	}
	rc, size, err := st.open(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	if st.key != nil {
		size = -1 // maybe decrypted
	}
	return rc, size, nil
}

// DeleteBackup deletes one of a server's backups.
func (s *Service) DeleteBackup(ctx context.Context, id, key, target string) error {
	st, name, err := s.backupKey(ctx, id, key, target)
	if err != nil {
		return err
	}
	s.mu.Lock()
	restoring := s.running[name] != nil && s.running[name].Key == key
	s.mu.Unlock()
	if restoring {
		return fmt.Errorf("this backup is being restored; %w", ErrRunning)
	}
	return st.remove(ctx, key)
}

// VerifyBackup reads one of a server's backups back, and records how it
// went.
func (s *Service) VerifyBackup(ctx context.Context, id, key, target string) (Verification, error) {
	st, name, err := s.backupKey(ctx, id, key, target)
	if err != nil {
		return Verification{}, err
	}
	// The file count is known for backups this plan's history has.
	files := -1
	if plans, err := s.plans(); err == nil {
		for _, r := range []*Run{plans[name].Last, plans[name].LastOK} {
			if r != nil && r.Key == key && target == "" {
				files = r.Files
			}
		}
	}
	v := st.verify(ctx, key, "", 0, files)
	s.recordCheck(name, st.t.ID, key, v)
	if !v.OK {
		s.notifyVerifyFailed(name, key, v)
	}
	return v, nil
}

// retryAfter is how soon a failed scheduled backup is tried again, unless
// the schedule itself is sooner.
const retryAfter = time.Hour

// due reports whether a plan's scheduled backup should run now.
func due(p Plan, now time.Time, loc *time.Location) bool {
	if len(p.Folders) == 0 {
		return false
	}
	if p.At == nil {
		return dueEvery(p.IntervalHours, p.Last, now)
	}
	// The first time of day after the last backup (or since the schedule
	// was set); a failed one is tried again after retryAfter.
	from := p.Since
	if p.Last != nil {
		if p.Last.Error != "" && !now.Before(p.Last.Started.Add(retryAfter)) {
			return true
		}
		from = p.Last.Started
	}
	if from.IsZero() {
		from = now.Add(-time.Minute)
	}
	next := p.At.next(from, loc)
	return !next.IsZero() && !now.Before(next)
}

// dueEvery reports whether a backup every intervalHours (0: never), last
// run as last, should run now.
func dueEvery(intervalHours int, last *Run, now time.Time) bool {
	if intervalHours <= 0 {
		return false
	}
	if last == nil {
		return true
	}
	wait := time.Duration(intervalHours) * time.Hour
	if last.Error != "" && retryAfter < wait {
		wait = retryAfter
	}
	return !now.Before(last.Started.Add(wait))
}

// RunScheduler starts scheduled backups as they come due, until ctx ends.
func (s *Service) RunScheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !s.Configured() {
			continue
		}
		plans, err := s.plans()
		if err != nil {
			s.log.Warn("read backup plans", "err", err)
			continue
		}
		now := time.Now()
		if s.config != nil {
			if cp, err := s.configPlan(); err == nil && dueEvery(cp.IntervalHours, cp.Last, now) {
				if err := s.startConfig("schedule"); err != nil && !errors.Is(err, ErrRunning) {
					s.log.Warn("start scheduled backup of hlm's configuration", "err", err)
				}
			}
		}
		s.checkStale(plans, now)
		for name, p := range plans {
			if !due(p, now, s.loc()) {
				continue
			}
			if _, err := s.servers.Inspect(ctx, name); err != nil {
				continue // deleted, or Docker is unreachable; try again later
			}
			if err := s.start(name, "schedule"); err != nil && !errors.Is(err, ErrRunning) {
				s.log.Warn("start scheduled backup", "server", name, "err", err)
			}
		}
	}
}

// OpenBackup opens one of a server's backups in the primary storage as the
// tar stream it holds (decrypted and decompressed).
func (s *Service) OpenBackup(ctx context.Context, id, key string) (io.ReadCloser, error) {
	st, _, err := s.backupKey(ctx, id, key, "")
	if err != nil {
		return nil, err
	}
	rc, _, err := st.open(ctx, key)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(rc)
	if err != nil {
		rc.Close()
		return nil, fmt.Errorf("read the backup: %w", err)
	}
	return struct {
		io.Reader
		io.Closer
	}{gz, rc}, nil
}
