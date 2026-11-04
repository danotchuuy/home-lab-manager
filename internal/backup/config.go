package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/s3"
)

// hlm's own configuration is backed up as <prefix>/_hlm/hlm-config-<time>.tar.gz
// (<prefix>/_gsh/gsh-config-<time>.tar.gz before the short name changed from
// gsh to hlm).
// Container names can't start with "_", so no server's backups go there.
const (
	secretConfigPlan = "backup-config-plan"
	configRunName    = "_hlm" // its key in Service.running
)

func configSeries(c Settings) series {
	return series{dir: c.prefix() + "/_hlm/", file: "hlm-config-", legacy: &series{dir: c.prefix() + "/_gsh/", file: "gsh-config-"}}
}

// configFiles are the files in the data directory that hold hlm's
// configuration. The feed cache is downloaded again, and the game data some
// installs keep in the data directory (appdata/) is backed up per server.
var configFiles = []string{"secrets/secrets.enc", "templates.json", "deleted-servers.json"}

// keyFile is the encryption key for secrets.enc, when HLM_SECRET_KEY isn't set.
const keyFile = "secrets/secret.key"

// ConfigPlan says when hlm backs up its own configuration.
type ConfigPlan struct {
	// IntervalHours is how often to back up; 0 means only when asked.
	IntervalHours int `json:"intervalHours"`
	// Keep is how many backups to keep; 0 keeps them all.
	Keep int `json:"keep"`
	// IncludeKey adds the key that decrypts the stored credentials, making
	// the backup enough to restore hlm on its own, but also letting anyone
	// who can read the bucket read them.
	IncludeKey bool `json:"includeKey"`
	Last       *Run `json:"last,omitempty"`
}

// defaultConfigPlan applies until the plan is first saved: a daily backup,
// kept for two weeks.
var defaultConfigPlan = ConfigPlan{IntervalHours: 24, Keep: 14}

// ConfigSource is where hlm's configuration lives.
type ConfigSource struct {
	// DataDir is HLM_DATA_DIR.
	DataDir string
	// KeyFromEnv is set when the secrets' key comes from HLM_SECRET_KEY, so
	// it isn't in the data directory.
	KeyFromEnv bool
	// Servers returns the managed servers and their settings, which live in
	// Docker labels rather than the data directory. It's saved as
	// servers.json, so they can be set up again on a new host.
	Servers func(ctx context.Context) (any, error)
}

// UseConfig turns on backups of hlm's own configuration.
func (s *Service) UseConfig(src ConfigSource) {
	s.config = &src
}

func (s *Service) configPlan() (ConfigPlan, error) {
	p := defaultConfigPlan
	_, err := s.secrets.Get(secretConfigPlan, &p)
	return p, err
}

func (s *Service) updateConfigPlan(fn func(*ConfigPlan)) error {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	p, err := s.configPlan()
	if err != nil {
		return err
	}
	fn(&p)
	return s.secrets.Set(secretConfigPlan, p)
}

// ConfigOverview is everything the System page shows about hlm's own
// backups.
type ConfigOverview struct {
	Configured bool        `json:"configured"` // backup storage is set up
	Plan       ConfigPlan  `json:"plan"`
	Running    *Progress   `json:"running,omitempty"`
	Backups    []s3.Object `json:"backups"`
	KeyFromEnv bool        `json:"keyFromEnv"`
	Error      string      `json:"error,omitempty"`
}

// ErrNoConfig is returned when UseConfig wasn't called.
var ErrNoConfig = errors.New("backups of hlm's configuration aren't available")

// ConfigOverview returns the plan, a running backup and the backups in the
// bucket, newest first.
func (s *Service) ConfigOverview(ctx context.Context) (ConfigOverview, error) {
	if s.config == nil {
		return ConfigOverview{}, ErrNoConfig
	}
	p, err := s.configPlan()
	if err != nil {
		return ConfigOverview{}, err
	}
	o := ConfigOverview{Plan: p, Backups: []s3.Object{}, KeyFromEnv: s.config.KeyFromEnv}
	s.mu.Lock()
	if r := s.running[configRunName]; r != nil {
		cp := *r
		o.Running = &cp
	}
	s.mu.Unlock()
	cl, c, err := s.client()
	if errors.Is(err, ErrNotConfigured) {
		return o, nil
	}
	o.Configured = true
	if err != nil {
		o.Error = err.Error()
		return o, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	objs, err := configSeries(c).list(ctx, cl)
	if err != nil {
		o.Error = err.Error()
		return o, nil
	}
	o.Backups = newestFirst(objs)
	return o, nil
}

// SetConfigPlan saves when hlm backs up its configuration.
func (s *Service) SetConfigPlan(in ConfigPlan) (ConfigPlan, error) {
	if s.config == nil {
		return ConfigPlan{}, ErrNoConfig
	}
	if in.IntervalHours < 0 || in.IntervalHours > 24*365 {
		return ConfigPlan{}, errors.New("the interval must be between 0 (off) and 8760 hours")
	}
	if in.Keep < 0 {
		return ConfigPlan{}, errors.New("the number of backups to keep can't be negative")
	}
	var out ConfigPlan
	err := s.updateConfigPlan(func(p *ConfigPlan) {
		p.IntervalHours, p.Keep, p.IncludeKey = in.IntervalHours, in.Keep, in.IncludeKey
		out = *p
	})
	return out, err
}

// StartConfig begins a backup of hlm's configuration in the background.
func (s *Service) StartConfig() error {
	return s.startConfig("manual")
}

func (s *Service) startConfig(trigger string) error {
	if s.config == nil {
		return ErrNoConfig
	}
	if _, _, err := s.client(); err != nil {
		return err
	}
	plan, err := s.configPlan()
	if err != nil {
		return err
	}
	prog, err := s.claim(configRunName, "backup", trigger)
	if err != nil {
		return err
	}
	go func() {
		run := s.runConfig(context.Background(), plan, prog)
		s.release(configRunName)
		if run.Error != "" {
			s.log.Warn("backup of hlm's configuration failed", "err", run.Error)
		} else {
			s.log.Info("backed up hlm's configuration", "key", run.Key)
		}
		if err := s.updateConfigPlan(func(p *ConfigPlan) { p.Last = &run }); err != nil {
			s.log.Warn("record configuration backup result", "err", err)
		}
		s.notifyBackup("", run)
	}()
	return nil
}

func (s *Service) runConfig(ctx context.Context, plan ConfigPlan, prog *Progress) Run {
	run := Run{Trigger: prog.Trigger, Started: prog.Started}
	fail := func(err error) Run {
		run.Finished, run.Error = time.Now(), err.Error()
		return run
	}
	cl, c, err := s.client()
	if err != nil {
		return fail(err)
	}
	s.setStage(prog, "uploading")
	data, err := s.configArchive(ctx, plan.IncludeKey, &run)
	if err != nil {
		return fail(err)
	}
	r := configSeries(c)
	key := r.newKey(run.Started)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	size, err := cl.Upload(ctx, key, bytes.NewReader(data), "application/gzip", map[string]string{"Hlm-Server": configRunName})
	if err != nil {
		return fail(err)
	}
	run.Key, run.Size = key, size
	if plan.Keep > 0 {
		s.setStage(prog, "cleaning")
		if err := prune(ctx, cl, r, plan.Keep); err != nil {
			run.Warnings = append(run.Warnings, "couldn't delete old backups: "+err.Error())
		}
	}
	run.Finished = time.Now()
	return run
}

// configArchive builds the tar.gz of hlm's configuration. Entries are named
// relative to the data directory, so extracting it there restores them.
func (s *Service) configArchive(ctx context.Context, includeKey bool, run *Run) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	now := time.Now()
	add := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
		run.Files++
		run.Bytes += int64(len(data))
		return nil
	}

	files := configFiles
	if includeKey && !s.config.KeyFromEnv {
		files = append([]string{keyFile}, files...)
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(s.config.DataDir, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // e.g. no templates made from images yet
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		mode := int64(0o644)
		if strings.HasPrefix(name, "secrets/") {
			mode = 0o600
		}
		if err := add(name, mode, data); err != nil {
			return nil, err
		}
	}

	if s.config.Servers != nil {
		servers, err := s.config.Servers(ctx)
		if err != nil {
			// Still worth keeping the rest.
			run.Warnings = append(run.Warnings, "couldn't list the servers for servers.json: "+err.Error())
		} else {
			data, err := json.MarshalIndent(servers, "", "  ")
			if err != nil {
				return nil, err
			}
			if err := add("servers.json", 0o644, append(data, '\n')); err != nil {
				return nil, err
			}
		}
	}
	if err := add("README.txt", 0o644, []byte(configReadme(includeKey && !s.config.KeyFromEnv, s.config.KeyFromEnv))); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func configReadme(hasKey, keyFromEnv bool) string {
	var b strings.Builder
	b.WriteString(`Home Lab Manager (hlm) configuration backup

To restore: stop hlm, extract this archive into its data directory
(HLM_DATA_DIR, /data in the Docker image), then start hlm again.

  secrets/secrets.enc   Integration settings and credentials (UniFi, proxy, Linode,
                        Cloudflare, backup storage), servers' port options and
                        backup plans. Encrypted with AES-256-GCM.
  templates.json        Templates made from Docker images.
  deleted-servers.json  Data folders of deleted servers, shown on the Host page.
  servers.json          Every server's settings. They live in Docker, so they
                        aren't restored by extracting; install each server again
                        from its template with these values. The game data itself
                        is in each server's own backups.
`)
	switch {
	case keyFromEnv:
		b.WriteString("\nsecrets.enc is encrypted with HLM_SECRET_KEY, which isn't in this backup.\nSet the same HLM_SECRET_KEY when restoring.\n")
	case hasKey:
		b.WriteString("\n  secrets/secret.key    The key that decrypts secrets.enc. Anyone with this\n                        backup can read the stored credentials.\n")
	default:
		b.WriteString("\nThe key that decrypts secrets.enc (secrets/secret.key) isn't in this backup.\nPut back your own copy of it when restoring, or the stored credentials\ncan't be read and have to be entered again.\n")
	}
	return b.String()
}

// configKey checks that key is one of the configuration backups.
func (s *Service) configKey(key string) (*s3.Client, error) {
	if s.config == nil {
		return nil, ErrNoConfig
	}
	cl, c, err := s.client()
	if err != nil {
		return nil, err
	}
	if !configSeries(c).has(key) {
		return nil, ErrBadKey
	}
	return cl, nil
}

// DownloadConfig opens one of the configuration backups.
func (s *Service) DownloadConfig(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	cl, err := s.configKey(key)
	if err != nil {
		return nil, 0, err
	}
	return cl.Get(ctx, key)
}

// DeleteConfigBackup deletes one of the configuration backups.
func (s *Service) DeleteConfigBackup(ctx context.Context, key string) error {
	cl, err := s.configKey(key)
	if err != nil {
		return err
	}
	return cl.Delete(ctx, key)
}
