// Command hlm is the Home Lab Manager backend: an HTTP API that deploys and
// manages game server containers from Unraid Community Applications templates.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/api"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/cftunnel"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/proxy"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/selfupdate"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	level := slog.LevelInfo
	if env("HLM_DEBUG", "") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			os.Stdout.WriteString(version + "\n")
			return
		case "reset-password":
			if err := resetPassword(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "hlm reset-password:", err)
				os.Exit(1)
			}
			return
		case selfupdate.HelperCommand:
			if err := runHelper(log, os.Args[2:]); err != nil {
				os.Exit(1)
			}
			return
		}
	}
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	listen := env("HLM_LISTEN", ":8080")
	dataDir := env("HLM_DATA_DIR", "./data")
	appdataRoot := env("HLM_APPDATA_ROOT", "")
	if appdataRoot == "" {
		abs, err := filepath.Abs(filepath.Join(dataDir, "appdata"))
		if err != nil {
			return err
		}
		appdataRoot = abs
		if _, err := os.Stat("/.dockerenv"); err == nil {
			// Bind mounts are resolved by the daemon on the host, so a path
			// inside this container would put game data somewhere unexpected.
			log.Warn("running in a container without HLM_APPDATA_ROOT; game data will go to this path on the docker host", "path", appdataRoot)
		}
	}
	refresh, err := time.ParseDuration(env("HLM_FEED_REFRESH", "6h"))
	if err != nil {
		return errors.New("HLM_FEED_REFRESH: " + err.Error())
	}
	updateCheck, err := time.ParseDuration(env("HLM_UPDATE_CHECK", "12h"))
	if err != nil {
		return errors.New("HLM_UPDATE_CHECK: " + err.Error())
	}
	wanCheck, err := time.ParseDuration(env("HLM_WAN_CHECK", "5m"))
	if err != nil {
		return errors.New("HLM_WAN_CHECK: " + err.Error())
	}

	dockerCfg := docker.ConfigFromEnv()
	dc, err := docker.NewClient(dockerCfg)
	if err != nil {
		return err
	}
	log.Info("starting hlm", "version", version)
	switch {
	case strings.HasPrefix(dc.Host(), "unix://"):
		log.Info("using docker socket", "host", dc.Host())
	case dockerCfg.UsesTLS():
		log.Info("using remote docker host over TLS", "host", dc.Host(), "verify", dockerCfg.TLSVerify)
		if !dockerCfg.TLSVerify {
			log.Warn("DOCKER_TLS_VERIFY is not set; the daemon's certificate will not be checked")
		}
	default:
		log.Warn("using remote docker host without TLS; anyone on the network path can control it", "host", dc.Host())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if v, err := dc.Version(ctx); err != nil {
		log.Warn("cannot reach docker daemon; will keep trying", "err", err)
	} else {
		log.Info("connected to docker", "version", v.Version, "api", v.APIVersion)
	}

	cat := catalog.New(env("HLM_FEED_URL", ""), filepath.Join(dataDir, "cache"), log)
	if err := cat.UseCustomStore(filepath.Join(dataDir, "templates.json")); err != nil {
		log.Warn("load templates made from images", "err", err)
	}
	if err := cat.LoadCache(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("load cached app feed", "err", err)
	} else if err == nil {
		log.Info("loaded cached app feed", "apps", cat.Count())
	}
	go cat.RunRefresher(ctx, refresh)

	// Host metrics come from procfs, so they only describe the Docker host
	// when it is this machine.
	hostReason := ""
	if !strings.HasPrefix(dc.Host(), "unix://") {
		hostReason = "the Docker host is remote (" + dc.Host() + "), so its CPU, memory, network and disks can't be read from here"
	}
	host := hostmetrics.New(hostmetrics.Config{
		ProcPath: env("HLM_HOST_PROC", "/proc"),
		RootPath: env("HLM_HOST_ROOT", ""),
	}, hostReason)
	go host.Run(ctx)

	// Remove finished self-update helpers now, and again shortly, since the
	// helper that started this container is still watching it at startup.
	go func() {
		for _, wait := range []time.Duration{0, 2 * time.Minute} {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if err := selfupdate.CleanupHelpers(ctx, dc); err != nil {
				log.Warn("remove old update helpers", "err", err)
			}
		}
	}()

	mgr := manager.New(dc, cat, appdataRoot, log)
	mgr.UseHostMetrics(host)
	mgr.UseDeletedStore(filepath.Join(dataDir, "deleted-servers.json"))

	// Credentials (e.g. for UniFi) are encrypted at rest; see package secrets.
	store, err := secrets.Open(filepath.Join(dataDir, "secrets"), env("HLM_SECRET_KEY", ""))
	if err != nil {
		return fmt.Errorf("open secrets store: %w", err)
	}
	fwd := forwarding.New(store, mgr, host.LANAddress, log)
	mgr.UseIPChecker(func(ctx context.Context, ip string, ignore []string) (string, string, error) {
		if !fwd.Configured() {
			return "", "", nil
		}
		res, err := fwd.CheckIP(ctx, ip, ignore)
		if err != nil {
			return "", "", err
		}
		return res.Conflict, res.Warning, nil
	})
	relay := proxy.New(store, mgr, log)
	relay.UseWANLookup(fwd.WANAddress)
	relay.UsePorts(fwd.ServerPorts)
	relay.UseDocker(dc, env("HLM_WIREGUARD_IMAGE", ""))
	go relay.UpgradeWireGuardHome(ctx)
	if base := env("HLM_LINODE_API", ""); base != "" {
		relay.UseLinodeAPI(base)
	}
	if wanCheck > 0 {
		go relay.RunWANWatcher(ctx, wanCheck)
	}
	// HLM_OIDC=off ignores the sign-in settings, to get back in if they
	// lock everyone out.
	oidcOff := strings.EqualFold(env("HLM_OIDC", ""), "off")
	if oidcOff {
		log.Warn("HLM_OIDC=off: ignoring the sign-in settings")
	}
	signIn := auth.New(store, oidcOff, log)
	tunnel := cftunnel.New(store, mgr, dc, env("HLM_CLOUDFLARED_IMAGE", ""), log)
	if base := env("HLM_CLOUDFLARE_API", ""); base != "" {
		tunnel.UseAPI(base)
	}
	go tunnel.RenameLegacyConnector(ctx)
	// The containers these run show up beside the servers, with their usage
	// and update checks.
	mgr.UseConnection(proxy.LabelWireGuard, manager.ConnectionKind{
		Kind: "wireguard", Title: "WireGuard tunnel", Page: "/proxy", Recreate: relay.RecreateWireGuardHome,
	})
	mgr.UseConnection(cftunnel.LabelConnector, manager.ConnectionKind{
		Kind: "cloudflared", Title: "Cloudflare Tunnel", Page: "/tunnel", Recreate: tunnel.RecreateConnector,
	})
	// Servers' addresses and ports can change when they're reconfigured;
	// keep their hostnames pointing at them.
	go tunnel.RunSync(ctx, 5*time.Minute)
	go mgr.RunStorageRefresher(ctx, 10*time.Minute)
	backups := backup.New(store, mgr, log)
	backups.UseConfig(backup.ConfigSource{
		DataDir:    dataDir,
		KeyFromEnv: env("HLM_SECRET_KEY", "") != "",
		Servers: func(ctx context.Context) (any, error) {
			list, err := mgr.List(ctx)
			if err != nil {
				return nil, err
			}
			// What it takes to install each server again.
			type saved struct {
				Name      string          `json:"name"`
				App       string          `json:"app"`
				Template  string          `json:"templateId"`
				Image     string          `json:"image"`
				Autostart bool            `json:"autostart"`
				Request   *deploy.Request `json:"request,omitempty"`
			}
			out := make([]saved, 0, len(list))
			for _, srv := range list {
				out = append(out, saved{srv.Name, srv.App, srv.TemplateID, srv.Image, srv.Autostart, srv.Request})
			}
			return out, nil
		},
	})
	go backups.RunScheduler(ctx)
	repo := env("HLM_UPDATE_REPO", "danotchuuy/home-lab-manager")
	updater := selfupdate.New(ctx, selfupdate.Config{
		Version: version,
		Repo:    repo,
		APIBase: env("HLM_UPDATE_API", ""),
		Image:   env("HLM_UPDATE_IMAGE", ""),
		DataDir: dataDir,
		Docker:  dc,
		Log:     log,
		Busy:    mgr.Busy,
		Restart: func(path string) error {
			// No graceful shutdown: the update response has already been
			// sent, Go opens sockets close-on-exec, and if exec fails the
			// current process must keep serving.
			log.Info("restarting into the updated binary", "path", path)
			return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
		},
	})
	// hlm's own container, if it runs in one, shows beside the connections.
	// Set before the update checker starts, which lists them.
	mgr.UseSelf(updater.ContainerID(), manager.ConnectionKind{Kind: "hlm", Title: "Home Lab Manager", Page: "/system"})
	if updateCheck > 0 {
		go updater.Run(ctx, updateCheck)
		go mgr.RunUpdateChecker(ctx, updateCheck)
	}

	groups := rbac.NewStore(store, log)
	handler := api.Handler(api.Options{
		Docker:      dc,
		Catalog:     cat,
		Manager:     mgr,
		Host:        host,
		Updater:     updater,
		Forwarding:  fwd,
		Proxy:       relay,
		Tunnel:      tunnel,
		Backup:      backups,
		Auth:        signIn,
		Version:     version,
		AppdataRoot: appdataRoot,
		Groups:      groups,
		Token:       env("HLM_TOKEN", ""),
		WebDir:      env("HLM_WEB_DIR", ""),
		Log:         log,
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listen, "appdata", appdataRoot)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// runHelper is the update helper container's entry point: it replaces the
// old hlm container with one on the new image. Args: old container ID, new
// image, old version, new version, old image config (base64 JSON).
func runHelper(log *slog.Logger, args []string) error {
	if len(args) != 5 {
		log.Error("usage: hlm " + selfupdate.HelperCommand + " <container> <image> <from> <to> <old-image>")
		return errors.New("bad arguments")
	}
	dc, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		log.Error("docker client", "err", err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return selfupdate.RunHelper(ctx, dc, env("HLM_DATA_DIR", "./data"), args[0], args[1], args[2], args[3], args[4],
		func(format string, a ...any) { log.Info(fmt.Sprintf(format, a...)) })
}

// resetPassword gives a local account a new random password, creating the
// account if needed, for when nobody can sign in. In Docker:
// docker exec home-lab-manager hlm reset-password admin
// With --admin, an existing account also becomes an admin.
func resetPassword(args []string) error {
	makeAdmin := slices.Contains(args, "--admin")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--admin" })
	if len(args) != 1 {
		return errors.New("usage: hlm reset-password <username> [--admin]")
	}
	dataDir := env("HLM_DATA_DIR", "./data")
	store, err := secrets.Open(filepath.Join(dataDir, "secrets"), env("HLM_SECRET_KEY", ""))
	if err != nil {
		return fmt.Errorf("open secrets store: %w", err)
	}
	quiet := slog.New(slog.DiscardHandler)
	service := auth.New(store, false, quiet)
	password, created, err := service.ResetPassword(args[0], makeAdmin)
	if err != nil {
		return err
	}
	what := "New password"
	if created {
		what = "Created the account; its password"
	}
	fmt.Printf("%s for %s: %s\nSign in with it, then change it under System → Sign-in.\n", what, args[0], password)
	if service.AccountRole(args[0]) != rbac.Admin {
		fmt.Printf("%s is a member, not an admin. Add --admin to make it one.\n", args[0])
	}
	return nil
}

// env returns the setting key (HLM_*), or else the GSH_* setting it was
// named before the short name changed from gsh to hlm, or else def.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := os.Getenv("GSH_" + strings.TrimPrefix(key, "HLM_")); v != "" {
		return v
	}
	return def
}
