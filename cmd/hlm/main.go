// Command hlm is the Home Lab Manager backend: an HTTP API that deploys and
// manages game server containers from Unraid Community Applications templates.
package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	// Time zone names work even where the system has no zone database.
	_ "time/tzdata"

	"github.com/danotchuuy/home-lab-manager/internal/api"
	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/autoupdate"
	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/cftunnel"
	"github.com/danotchuuy/home-lab-manager/internal/cleanup"
	"github.com/danotchuuy/home-lab-manager/internal/configfiles"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/health"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/hosts"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/metrics"
	"github.com/danotchuuy/home-lab-manager/internal/metricstore"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/prefs"
	"github.com/danotchuuy/home-lab-manager/internal/proxy"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/reach"
	"github.com/danotchuuy/home-lab-manager/internal/registry"
	"github.com/danotchuuy/home-lab-manager/internal/schedule"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/selfupdate"
	"github.com/danotchuuy/home-lab-manager/internal/stack"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
	"github.com/danotchuuy/home-lab-manager/internal/tlscert"
	"github.com/danotchuuy/home-lab-manager/internal/transfer"
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
		case "ssh-login":
			if err := sshLoginCommand(os.Args[2:], os.Stdout, os.Stderr); err != nil {
				if !errors.Is(err, flag.ErrHelp) {
					fmt.Fprintln(os.Stderr, "hlm ssh-login:", err)
				}
				os.Exit(1)
			}
			return
		case "decrypt-backup":
			if err := decryptBackup(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "hlm decrypt-backup:", err)
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
	// HLM_UPDATE_CHECK, when set, wins over the setting on the System page.
	updateCheckEnv := env("HLM_UPDATE_CHECK", "")
	updateCheck, err := time.ParseDuration(cmp.Or(updateCheckEnv, "12h"))
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
	groups := rbac.NewStore(store, log)
	// Pulls and update checks sign in to private registries with these.
	registries := registry.New(store)
	dc.SetCredentials(registries.For)
	// Alerts about crashes, failed backups, updates and the like. Links in
	// them point at the HTTPS address, once certs (below) is set up.
	var certs *tlscert.Service
	var sched *schedule.Service // set before notifications are sent
	notifier := notify.New(notify.Options{
		Location: func() *time.Location {
			if sched == nil {
				return time.Local
			}
			return sched.Location()
		},
		Secrets:   store,
		StatePath: filepath.Join(dataDir, "notifications.json"),
		Log:       log,
		BaseURL:   func() string { return httpsAddress(certs, listen) },
		GroupServers: func(id string) []string {
			list, err := groups.Groups()
			if err != nil {
				return nil
			}
			for _, g := range list {
				if g.ID == id {
					return g.Servers
				}
			}
			return nil
		},
	})
	mgr.UseNotifier(notifier)
	go host.WatchDisks(ctx, 5*time.Minute, func() int {
		set, err := notifier.Settings()
		if err != nil {
			return notify.DefaultDiskPercent
		}
		return set.DiskThreshold()
	}, notifier)
	// Who changed what: API requests, sign-ins, and hlm's own actions.
	auditLog, err := audit.Open(filepath.Join(dataDir, "audit"), store, log)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	go auditLog.Run(ctx)
	// Crash-loop limits and custom health checks, per server.
	healthStore := health.NewStore(store)
	mgr.UseHealthPolicy(healthStore)
	mgr.UseAudit(auditLog)
	if err := mgr.UseCrashLoopStore(filepath.Join(dataDir, "crash-loops.json")); err != nil {
		log.Warn("read servers stopped for crash loops", "err", err)
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
	relay.UseNotifier(notifier)
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
	signIn.UseNotifier(notifier)
	signIn.UseAudit(auditLog)
	tunnel := cftunnel.New(store, mgr, dc, env("HLM_CLOUDFLARED_IMAGE", ""), log)
	if base := env("HLM_CLOUDFLARE_API", ""); base != "" {
		tunnel.UseAPI(base)
	}
	go tunnel.RenameLegacyConnector(ctx)
	// Other Docker hosts, whose servers hlm manages too. They sign in over
	// SSH with the same key as the proxy.
	dockerHosts := hosts.New(hosts.Options{
		Secrets: store, SSHKey: relay.KeyPair, Credentials: registries.For, OnChange: mgr.SetHosts, Log: log,
	})
	if err := dockerHosts.Load(); err != nil {
		log.Warn("load Docker hosts", "err", err)
	}
	defer dockerHosts.Close()
	fwd.UseHostAddress(mgr.HostAddress)
	tunnel.UseHostAddress(mgr.HostAddress)
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
	backups.UseNotifier(notifier)
	backups.UseAudit(auditLog)
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
	updater.UseNotifier(notifier)
	// Tunnel routes to hlm itself (status page sites) go to its port, as
	// its container publishes it if it runs in one.
	selfOrigin := func(ctx context.Context) (cftunnel.SelfOrigin, error) {
		_, p, err := net.SplitHostPort(listen)
		if err != nil {
			return cftunnel.SelfOrigin{}, err
		}
		port, _ := strconv.Atoi(p)
		so := cftunnel.SelfOrigin{Port: port}
		if id := updater.ContainerID(); id != "" {
			info, err := dc.InspectContainer(ctx, id)
			if err != nil {
				return so, err
			}
			so.Container = &info
		}
		return so, nil
	}
	tunnel.UseSelf(selfOrigin)
	// After UseConnection and UseSelf, which say which containers are whose.
	go mgr.WatchEvents(ctx)
	if updateCheck > 0 {
		go updater.Run(ctx, updateCheck)
	}

	// HTTPS is optional, set up on the System page; HLM_TLS=off ignores the
	// setting, to get back in over plain HTTP if it goes wrong.
	tlsOff := strings.EqualFold(env("HLM_TLS", ""), "off")
	if tlsOff {
		log.Warn("HLM_TLS=off: serving plain HTTP only")
	}
	acmeClient, err := acmeHTTPClient(env("HLM_ACME_CA", ""))
	if err != nil {
		return fmt.Errorf("HLM_ACME_CA: %w", err)
	}
	certs = tlscert.New(tlscert.Options{
		Secrets: store, DNS: tunnel, Log: log, Off: tlsOff,
		Directory: env("HLM_ACME_DIRECTORY", ""), HTTPClient: acmeClient, Resolver: env("HLM_ACME_RESOLVER", ""),
	})
	certs.UseNotifier(notifier)
	go certs.Run(ctx)
	// Commands typed into servers' consoles.
	// Where the Docker host's published ports are, seen from here. In a
	// container, that's the gateway of hlm's bridge network first: on a
	// macvlan network too, hlm can't reach the host's LAN address.
	var (
		gatewayMu sync.Mutex
		gateways  []string
		gatewayOK bool
	)
	hostAddresses := func(id string) []string {
		// A server on another Docker host is reached at that host.
		if a := mgr.HostAddress(id); a != "" {
			return []string{a}
		}
		if !strings.HasPrefix(dc.Host(), "unix://") {
			if u, err := url.Parse(dc.Host()); err == nil {
				return []string{u.Hostname()}
			}
		}
		self := updater.ContainerID()
		if self == "" {
			return []string{"127.0.0.1", host.LANAddress()}
		}
		gatewayMu.Lock()
		if !gatewayOK {
			gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			g, err := reach.HostGateways(gctx, dc, self)
			cancel()
			if err == nil {
				gateways, gatewayOK = g, true // networks change only when hlm's container is recreated
			}
		}
		out := slices.Clone(gateways)
		gatewayMu.Unlock()
		return append(out, host.LANAddress())
	}
	consoles := console.New(console.Options{
		Secrets: store, Servers: mgr, Docker: mgr.Docker(), HostAddresses: hostAddresses, Log: log,
	})
	go consoles.Run(ctx)
	// Games that don't save when told to stop (Palworld) are saved first.
	mgr.UseBeforeStop(func(ctx context.Context, id string) { consoles.SaveBeforeStop(ctx, id, 30*time.Second) })
	// Servers' own status: are they answering, and who's playing.
	queries := gamequery.New(gamequery.Options{
		Secrets: store, Servers: mgr, Console: consoles, Notifier: notifier, HostAddresses: hostAddresses, Log: log,
	})
	go queries.Run(ctx)
	// A month of host and server resource use, and /metrics.
	history, err := metricstore.Open(filepath.Join(dataDir, "metrics"), log)
	if err != nil {
		return fmt.Errorf("open metrics history: %w", err)
	}
	defer func() {
		if err := history.Close(); err != nil {
			log.Warn("save metrics history", "err", err)
		}
	}()
	go history.Run(ctx)
	sampler := metrics.New(history, mgr, queries, log)
	host.Observe(sampler.ObserveHost)
	go sampler.Run(ctx)
	// Servers' scheduled tasks, such as a nightly restart.
	sched = schedule.New(schedule.Options{
		Secrets: store, Servers: mgr, Backups: backups, Console: consoles, Notifier: notifier,
		Players: queries.Players, Audit: auditLog, Log: log,
	})
	go sched.Run(ctx)
	// Backups' console commands before and after, and their schedules and
	// retention in hlm's time zone.
	backups.UseConsole(func(ctx context.Context, server, command string) error {
		_, err := consoles.Send(ctx, server, "hlm (backup)", command)
		return err
	})
	backups.UseLocation(sched.Location)
	// Updates applied automatically, for servers (and hlm) that want them.
	autoUpdates := autoupdate.New(autoupdate.Options{
		Secrets: store, Servers: mgr, Backups: backups, Console: consoles, Tasks: sched, Self: updater,
		Notifier: notifier, Players: queries.Players, Audit: auditLog, Location: sched.Location, ZoneName: sched.ZoneName, Log: log,
	})
	go autoUpdates.Run(ctx)
	// Servers whose health keeps failing are restarted, if they want that.
	healthMonitor := health.NewMonitor(health.Options{
		Store: healthStore, Servers: mgr, Responding: queries.Responding, Busy: backups.Busy,
		Notifier: notifier, Audit: auditLog, Log: log,
	})
	go healthMonitor.Run(ctx)
	checkEvery := autoUpdates.CheckInterval
	if updateCheckEnv != "" {
		checkEvery = func() time.Duration { return updateCheck }
	}
	go mgr.RunUpdateChecker(ctx, checkEvery)
	go notifier.Run(ctx)
	pinned := configfiles.New(store, mgr)
	// Servers exported to a file, imported from one, and cloned.
	transfers := transfer.New(transfer.Options{
		Manager: mgr, Catalog: cat, Backups: backups, Tasks: sched, Updates: autoUpdates, Query: queries,
		Console: consoles, ConfigFiles: pinned, Health: healthStore, Version: version, Log: log,
	})
	// Old images and leftovers: deleted by hand from the Host page, after
	// updates, and weekly if that's on.
	cleaner := cleanup.New(store, mgr, auditLog, log)
	mgr.UseOldImageRemoval(cleaner.RemoveOldImages)
	go cleaner.Run(ctx)
	// The public status page, with addresses suggested from the proxy and
	// UniFi's port forwards.
	statusPage := statuspage.New(statuspage.Options{
		Secrets: store, Servers: mgr, Query: queries,
		Uptime:       func(name string, rng time.Duration) (float64, bool) { return metrics.Uptime(history, name, rng) },
		Suggest:      suggestAddresses(mgr, fwd, relay),
		HashPassword: auth.HashPassword, PasswordMatches: auth.PasswordMatches,
		Groups: func() (map[string][]string, error) {
			list, err := groups.Groups()
			if err != nil {
				return nil, err
			}
			out := make(map[string][]string, len(list))
			for _, g := range list {
				out[g.ID] = g.Servers
			}
			return out, nil
		},
	})
	// The proxy server can serve the status page's sites too, through the
	// WireGuard tunnel to hlm's port on this host.
	// Cloudflare in front of them, when that's their HTTPS option.
	relay.UseCloudflare(tunnel)
	relay.UseStatusSites(func(ctx context.Context) ([]string, int, error) {
		set, err := statusPage.Settings()
		if err != nil {
			return nil, 0, err
		}
		var domains []string
		if set.Enabled {
			for _, site := range set.Sites {
				domains = append(domains, site.Hostname)
			}
		}
		so, err := selfOrigin(ctx)
		if err != nil {
			return nil, 0, err
		}
		port, err := hostPortOf(so)
		return domains, port, err
	})
	// Docker Compose stacks.
	stacks := stack.New(store, dc, mgr, log)
	handler := api.Handler(api.Options{
		Docker:        dc,
		Catalog:       cat,
		Manager:       mgr,
		Host:          host,
		Updater:       updater,
		Forwarding:    fwd,
		Proxy:         relay,
		Tunnel:        tunnel,
		Backup:        backups,
		Auth:          signIn,
		Version:       version,
		AppdataRoot:   appdataRoot,
		Groups:        groups,
		HTTPS:         certs,
		Notify:        notifier,
		Console:       consoles,
		Schedule:      sched,
		AutoUpdate:    autoUpdates,
		Query:         queries,
		ConfigFiles:   pinned,
		Audit:         auditLog,
		Registries:    registries,
		History:       history,
		Sampler:       sampler,
		MetricsConfig: metrics.NewConfig(store),
		Health:        healthStore,
		HealthMonitor: healthMonitor,
		Transfer:      transfers,
		Stacks:        stacks,
		Hosts:         dockerHosts,
		Cleanup:       cleaner,
		Prefs:         prefs.New(store),
		StatusPage:    statusPage,
		// Set when HLM_UPDATE_CHECK overrides the setting.
		UpdateCheckEnv: updateCheckEnv,
		Token:          env("HLM_TOKEN", ""),
		WebDir:         env("HLM_WEB_DIR", ""),
		Log:            log,
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           statusSitesFirst(statusPage, certs.Redirect(handler), handler),
		ReadHeaderTimeout: 10 * time.Second,
		// Close idle keep-alive connections, so that turning HTTPS off (or
		// on) reaches browsers within a couple of minutes.
		IdleTimeout: 2 * time.Minute,
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listen, "appdata", appdataRoot, "https", certs.Active())
		// HTTPS and HTTP share the port; see tlscert.Listener.
		errc <- srv.Serve(certs.Listener(ln))
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
// With --admin, an existing account also becomes an admin; with
// --reset-2fa, its two-factor sign-in is turned off.
func resetPassword(args []string) error {
	makeAdmin := slices.Contains(args, "--admin")
	reset2FA := slices.Contains(args, "--reset-2fa")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--admin" || a == "--reset-2fa" })
	if len(args) != 1 {
		return errors.New("usage: hlm reset-password <username> [--admin] [--reset-2fa]")
	}
	dataDir := env("HLM_DATA_DIR", "./data")
	store, err := secrets.Open(filepath.Join(dataDir, "secrets"), env("HLM_SECRET_KEY", ""))
	if err != nil {
		return fmt.Errorf("open secrets store: %w", err)
	}
	quiet := slog.New(slog.DiscardHandler)
	service := auth.New(store, false, quiet)
	password, created, err := service.ResetPassword(args[0], makeAdmin, reset2FA)
	if err != nil {
		return err
	}
	if l, err := audit.Open(filepath.Join(dataDir, "audit"), store, quiet); err == nil {
		action := "auth.reset_password"
		if created {
			action = "auth.accounts.create"
		}
		details := map[string]string{"via": "hlm reset-password"}
		if makeAdmin {
			details["admin"] = "yes"
		}
		if reset2FA {
			details["twoFactor"] = "reset"
		}
		l.Record(audit.Entry{Actor: "host", ActorKind: audit.ActorHost, Action: action, Target: args[0], Details: details})
		l.Close()
	}
	what := "New password"
	if created {
		what = "Created the account; its password"
	}
	fmt.Printf("%s for %s: %s\nSign in with it, then change it under System → Sign-in.\n", what, args[0], password)
	if service.AccountRole(args[0]) != rbac.Admin {
		fmt.Printf("%s is a member, not an admin. Add --admin to make it one.\n", args[0])
	}
	switch {
	case reset2FA:
		fmt.Printf("Two-factor sign-in is off for %s; if it's required, it's set up again when signing in.\n", args[0])
	case service.HasTwoFactor(args[0]):
		fmt.Printf("%s still uses two-factor sign-in: add --reset-2fa if its authenticator app is lost.\n", args[0])
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

// acmeHTTPClient talks to the ACME CA, trusting the extra CA certificates in
// caFile if it's set: for a private ACME CA, like step-ca.
func acmeHTTPClient(caFile string) (*http.Client, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	if caFile == "" {
		return c, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s has no PEM certificates", caFile)
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c, nil
}

// httpsAddress is hlm's address when it serves HTTPS, for links in
// notifications, or "" when it doesn't.
func httpsAddress(certs *tlscert.Service, listen string) string {
	if certs == nil || !certs.Active() {
		return ""
	}
	host := certs.Settings().Domain
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "443" && port != "" {
		host = net.JoinHostPort(host, port)
	}
	return "https://" + host
}

// decryptBackup decrypts an encrypted backup: hlm decrypt-backup KEY < in > out,
// or with -key-file and file names.
func decryptBackup(args []string) error {
	fs := flag.NewFlagSet("decrypt-backup", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "read the key from this file instead of the first argument")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: hlm decrypt-backup [-key-file FILE | KEY] [IN [OUT]]")
		fmt.Fprintln(fs.Output(), "Decrypts a backup from encrypted backup storage (stdin to stdout by default).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	key := ""
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		key = string(data)
	} else {
		if len(rest) == 0 {
			fs.Usage()
			return errors.New("give the key")
		}
		key, rest = rest[0], rest[1:]
	}
	in, out := io.Reader(os.Stdin), io.Writer(os.Stdout)
	if len(rest) > 0 {
		f, err := os.Open(rest[0])
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	if len(rest) > 1 {
		f, err := os.OpenFile(rest[1], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	return backup.Decrypt(out, in, key)
}

// hostPortOf is the port hlm is reached on at the Docker host: its own, or
// the one its container publishes for it.
func hostPortOf(so cftunnel.SelfOrigin) (int, error) {
	info := so.Container
	if info == nil || info.HostConfig.NetworkMode == "host" {
		return so.Port, nil
	}
	for _, b := range info.HostConfig.PortBindings[strconv.Itoa(so.Port)+"/tcp"] {
		if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
			return p, nil
		}
	}
	return 0, fmt.Errorf("hlm's container doesn't publish its port %d on the host, so the WireGuard tunnel can't reach it: publish it (-p %d:%d)", so.Port, so.Port, so.Port)
}

// statusSitesFirst sends requests for the status page's sites straight to
// the handler: they come through a proxy or tunnel, which may speak plain
// HTTP to hlm, and mustn't be redirected to hlm's own HTTPS address.
func statusSitesFirst(sp *statuspage.Service, redirected, direct http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sp.IsSiteHost(r.Host) {
			direct.ServeHTTP(w, r)
			return
		}
		redirected.ServeHTTP(w, r)
	})
}
