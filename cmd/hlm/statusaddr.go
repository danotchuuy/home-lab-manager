package main

import (
	"context"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/proxy"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// suggestAddresses offers the status page addresses players could use for
// a server: the proxy server's, for ports it relays, and the home
// network's public address from UniFi, for ports it forwards.
func suggestAddresses(mgr *manager.Manager, fwd *forwarding.Service, relay *proxy.Service) func(context.Context, string) []statuspage.Suggestion {
	var (
		mu      sync.Mutex
		wan     string
		wanTime time.Time
	)
	wanAddress := func(ctx context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		if wan != "" && time.Since(wanTime) < 10*time.Minute {
			return wan
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if a, err := fwd.WANAddress(cctx, ""); err == nil {
			wan, wanTime = a, time.Now()
		}
		return wan
	}
	return func(ctx context.Context, name string) []statuspage.Suggestion {
		info, err := mgr.Inspect(ctx, name)
		if err != nil {
			return nil
		}
		ports, err := fwd.ServerPorts(info)
		if err != nil {
			return nil
		}
		var out []statuspage.Suggestion
		add := func(host string, port int, source string) {
			a := statuspage.Suggestion{Address: net.JoinHostPort(host, strconv.Itoa(port)), Source: source}
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
		if set, ok, err := relay.Settings(); err == nil && ok && set.Host != "" && slices.Contains(set.Servers, name) {
			for _, m := range ports {
				if m.Proxy {
					add(set.Host, m.Port, "proxy")
				}
			}
		}
		if fwd.Configured() {
			if a := wanAddress(ctx); a != "" {
				for _, m := range ports {
					if m.Forward {
						add(a, m.Port, "unifi")
					}
				}
			}
		}
		return out
	}
}
