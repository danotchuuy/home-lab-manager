package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// The status page's sites on the proxy server: nginx serves each site's
// domain over HTTP (and HTTPS, with Let's Encrypt certificates certbot
// gets there), passing only the status page's paths to hlm through the
// WireGuard tunnel. hlm's own port isn't open to the internet: the tunnel
// reaches it from the proxy server alone, and both nginx and hlm turn away
// everything but the status page on those domains.

// StatusConfigPath is the nginx file for the status page's sites, in the
// http block's conf.d.
const StatusConfigPath = "/etc/nginx/conf.d/hlm-status.conf"

// acmeRoot is where certbot puts its challenges for nginx to serve.
const acmeRoot = "/var/www/hlm-acme"

const statusRuleLabel = rulePrefix + "status"

// secretStatusServed records that the status page's configuration is on
// the proxy, so turning it off removes it.
const secretStatusServed = "proxy-status-served"

// secretStatusResult keeps the last outcome, for the status page's settings.
const secretStatusResult = "proxy-status-result"

// StatusSites returns how serving the status page's sites last went, if
// it's on and has been applied.
func (s *Service) StatusSites() (*StatusSiteResult, error) {
	var r StatusSiteResult
	ok, err := s.secrets.Get(secretStatusResult, &r)
	if err != nil || !ok {
		return nil, err
	}
	return &r, nil
}

// UseStatusSites says which domains the status page's sites use, and the
// port hlm is reached on at the Docker host.
func (s *Service) UseStatusSites(f func(ctx context.Context) (domains []string, port int, err error)) {
	s.statusSites = f
}

// StatusSiteResult is how serving the status page's sites went.
type StatusSiteResult struct {
	Domains []string `json:"domains"`
	// HTTPS lists the domains with a certificate, served over HTTPS.
	HTTPS []string `json:"https"`
	// Failed says why certificates couldn't be got, by domain.
	Failed map[string]string `json:"failed,omitempty"`
	Output string            `json:"output,omitempty"`
	Error  string            `json:"error,omitempty"`
}

var statusEmail = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}\.[A-Za-z]{2,}$`)

var statusDomain = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// statusLocations are what nginx passes to hlm for the status page: the
// page, its API and the files it loads. Anything else is 404 at the proxy.
const statusLocations = `    location ~ ^/(status$|api/status-page|assets/|favicon\.|icon-[0-9a-z-]+\.png$|apple-touch-icon\.png$|manifest\.webmanifest$) {
        proxy_pass http://%s;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
        proxy_read_timeout 30s;
    }
    location = / {
        return 302 /status;
    }
    location / {
        return 404;
    }
`

// RenderStatus produces the nginx http configuration for the status page's
// sites: each domain on port 80 (answering Let's Encrypt's challenges, and
// redirecting to HTTPS once it has a certificate) and, with one, on 443.
func RenderStatus(domains []string, certs map[string]bool, upstream string, port int, ipv6 bool) string {
	var b strings.Builder
	b.WriteString("# Managed by Home Lab Manager; changes made here are overwritten.\n")
	b.WriteString("# The status page's sites, passed to hlm through the WireGuard tunnel.\n")
	if len(domains) == 0 {
		return b.String()
	}
	if a, err := netip.ParseAddr(upstream); err == nil && a.Is6() {
		upstream = "[" + upstream + "]"
	}
	to := upstream + ":" + strconv.Itoa(port)
	locations := fmt.Sprintf(statusLocations, to)
	listen := func(port string, extra string) string {
		l := "    listen " + port + extra + ";\n"
		if ipv6 {
			l += "    listen [::]:" + port + extra + ";\n"
		}
		return l
	}
	for _, d := range domains {
		fmt.Fprintf(&b, "\n# %s\nserver {\n%s    server_name %s;\n    access_log off;\n", d, listen("80", ""), d)
		fmt.Fprintf(&b, "    location ^~ /.well-known/acme-challenge/ {\n        root %s;\n    }\n", acmeRoot)
		if certs[d] {
			b.WriteString("    location / {\n        return 301 https://$host$request_uri;\n    }\n}\n")
			fmt.Fprintf(&b, "server {\n%s    server_name %s;\n    access_log off;\n", listen("443", " ssl"), d)
			fmt.Fprintf(&b, "    ssl_certificate /etc/letsencrypt/live/%s/fullchain.pem;\n", d)
			fmt.Fprintf(&b, "    ssl_certificate_key /etc/letsencrypt/live/%s/privkey.pem;\n", d)
		}
		b.WriteString(locations)
		b.WriteString("}\n")
	}
	return b.String()
}

// statusProbeScript says whether nginx's http block reads conf.d, whether
// IPv6 is there, and which of the domains (on stdin, one per line) have a
// certificate.
const statusProbeScript = `PATH="$PATH:/usr/sbin:/sbin"
dump=$(nginx -T 2>/dev/null)
printf '%s\n' "$dump" | grep -qE 'include[[:space:]]+/etc/nginx/conf\.d/\*\.conf' && echo "hlm:confd"
[ -f /proc/net/if_inet6 ] && echo "hlm:ipv6"
command -v certbot >/dev/null 2>&1 && echo "hlm:certbot"
while IFS= read -r d; do
  [ -n "$d" ] && [ -f "/etc/letsencrypt/live/$d/fullchain.pem" ] && echo "hlm:cert:$d"
done
true`

// statusApplyScript installs the configuration read from stdin, and
// reloads nginx if "nginx -t" passes; otherwise the previous file is put
// back.
const statusApplyScript = `PATH="$PATH:/usr/sbin:/sbin"
conf=` + StatusConfigPath + `
mkdir -p ` + acmeRoot + ` /etc/nginx/conf.d
tmp=$(mktemp) || exit 1
cat > "$tmp"
had=0
if [ -f "$conf" ]; then had=1; cp -p "$conf" "$conf.bak"; fi
install -m 644 "$tmp" "$conf"; rm -f "$tmp"
if ! out=$(nginx -t 2>&1); then
  echo "$out"
  if [ $had = 1 ]; then mv "$conf.bak" "$conf"; else rm -f "$conf"; fi
  echo "hlm:testfailed"
  exit 4
fi
if [ -d /run/systemd/system ] && systemctl cat nginx >/dev/null 2>&1; then
  systemctl reload-or-restart nginx || exit 5
elif ! nginx -s reload 2>/dev/null; then
  nginx || exit 5
fi
echo "hlm:applied"`

// certbotScript gets a certificate for one domain ($1), with the email
// address $2 (or none), through the challenges nginx serves from acmeRoot.
// certbot is installed first if it isn't there; its own timer renews the
// certificate, reloading nginx.
const certbotScript = `PATH="$PATH:/usr/sbin:/sbin"
if ! command -v certbot >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q certbot >/dev/null || exit 2
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y certbot >/dev/null || exit 2
  else
    echo "install certbot on the proxy server" >&2; exit 2
  fi
fi
if [ -n "$2" ]; then account="-m $2"; else account="--register-unsafely-without-email"; fi
certbot certonly --webroot -w ` + acmeRoot + ` -d "$1" --non-interactive --agree-tos $account \
  --deploy-hook "nginx -s reload" --keep-until-expiring 2>&1 | tail -n 5`

// statusProbe is what statusProbeScript found.
type statusProbe struct {
	confd, ipv6, certbot bool
	certs                map[string]bool
}

func (s *Service) probeStatus(ctx context.Context, domains []string) (statusProbe, error) {
	res, err := s.run(ctx, statusProbeScript, []byte(strings.Join(domains, "\n")+"\n"), nil)
	if err != nil {
		return statusProbe{}, err
	}
	p := statusProbe{certs: map[string]bool{}}
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch line = strings.TrimSpace(line); {
		case line == "hlm:confd":
			p.confd = true
		case line == "hlm:ipv6":
			p.ipv6 = true
		case line == "hlm:certbot":
			p.certbot = true
		case strings.HasPrefix(line, "hlm:cert:"):
			p.certs[strings.TrimPrefix(line, "hlm:cert:")] = true
		}
	}
	return p, nil
}

// writeStatus installs a status configuration on the proxy.
func (s *Service) writeStatus(ctx context.Context, config string) (string, error) {
	res, err := s.run(ctx, statusApplyScript, []byte(config), nil)
	out := strings.TrimSpace(strings.NewReplacer("hlm:applied", "", "hlm:testfailed", "").Replace(res.Output()))
	switch {
	case err != nil:
		return out, err
	case strings.Contains(res.Stdout, "hlm:applied"):
		return out, nil
	case strings.Contains(res.Stdout, "hlm:testfailed"):
		return out, errors.New("nginx rejected the status page's configuration, so the previous one was kept")
	default:
		return out, fmt.Errorf("applying the status page's configuration failed (exit status %d)", res.ExitCode)
	}
}

// applyStatus serves the status page's sites on the proxy, as the settings
// say: their domains to hlm through the WireGuard tunnel, with HTTPS
// certificates if that's on. Without them, it removes the configuration.
// The caller holds applyMu.
func (s *Service) applyStatus(ctx context.Context, st Settings) *StatusSiteResult {
	var served bool
	_, _ = s.secrets.Get(secretStatusServed, &served)
	if !st.StatusPage {
		// Turned off: take the sites off the proxy, once.
		if served {
			if _, err := s.writeStatus(ctx, RenderStatus(nil, nil, "", 0, false)); err != nil {
				s.log.Warn("clear the status page's configuration on the proxy", "err", err)
			} else {
				_ = s.secrets.Delete(secretStatusServed)
			}
		}
		return nil
	}
	if !served {
		_ = s.secrets.Set(secretStatusServed, true)
	}
	res := &StatusSiteResult{Domains: []string{}, HTTPS: []string{}}
	fail := func(err error) *StatusSiteResult {
		res.Error = err.Error()
		return res
	}
	if st.UpstreamSource != "wireguard" {
		return fail(errors.New("the status page goes through the WireGuard tunnel, so hlm's own port isn't open to the internet: set the tunnel up and use it as the home address"))
	}
	if s.statusSites == nil {
		return fail(errors.New("hlm can't tell the status page's sites here"))
	}
	domains, port, err := s.statusSites(ctx)
	if err != nil {
		return fail(err)
	}
	for _, d := range domains {
		if statusDomain.MatchString(d) && !slices.Contains(res.Domains, d) {
			res.Domains = append(res.Domains, d)
		}
	}
	slices.Sort(res.Domains)
	if port == 0 {
		return fail(errors.New("hlm can't tell which port it's reached on at the Docker host"))
	}
	probe, err := s.probeStatus(ctx, res.Domains)
	if err != nil {
		return fail(err)
	}
	if !probe.confd && len(res.Domains) > 0 {
		return fail(errors.New("nginx on the proxy server doesn't read /etc/nginx/conf.d/*.conf in its http block, where hlm puts the status page's sites; add that include to nginx.conf"))
	}
	render := func() string { return RenderStatus(res.Domains, probe.certs, st.Upstream, port, probe.ipv6) }
	if res.Output, err = s.writeStatus(ctx, render()); err != nil {
		return fail(err)
	}
	// Certificates for the domains without one, now that nginx answers
	// their challenges; then HTTPS for those.
	if st.StatusHTTPS {
		got := false
		for _, d := range res.Domains {
			if probe.certs[d] {
				continue
			}
			out, err := s.run(ctx, "sh -s -- "+remote.Quote(d)+" "+remote.Quote(st.StatusEmail)+" <<'HLM'\n"+certbotScript+"\nHLM", nil, nil)
			switch {
			case err != nil:
				res.setFailed(d, err.Error())
			case out.ExitCode != 0:
				res.setFailed(d, lastLine(out.Output()))
			default:
				probe.certs[d], got = true, true
			}
		}
		if got {
			if res.Output, err = s.writeStatus(ctx, render()); err != nil {
				return fail(err)
			}
		}
	}
	for _, d := range res.Domains {
		if probe.certs[d] {
			res.HTTPS = append(res.HTTPS, d)
		}
	}
	s.log.Info("served the status page's sites on the proxy", "domains", len(res.Domains), "https", len(res.HTTPS))
	return res
}

// applyStatusRecorded is applyStatus, keeping its outcome.
func (s *Service) applyStatusRecorded(ctx context.Context, st Settings) *StatusSiteResult {
	res := s.applyStatus(ctx, st)
	if res == nil {
		_ = s.secrets.Delete(secretStatusResult)
	} else if err := s.secrets.Set(secretStatusResult, res); err != nil {
		s.log.Warn("save the status page's result", "err", err)
	}
	return res
}

func (r *StatusSiteResult) setFailed(domain, why string) {
	if r.Failed == nil {
		r.Failed = map[string]string{}
	}
	r.Failed[domain] = why
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// statusRule opens HTTP and HTTPS on the proxy's firewall, for the status
// page's sites.
func statusRule() linode.Rule {
	return linode.Rule{
		Action: "ACCEPT", Protocol: "TCP", Ports: "80,443",
		Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}, IPv6: []string{"::/0"}},
		Label:     statusRuleLabel, Description: "Home Lab Manager: the status page's sites",
	}
}
