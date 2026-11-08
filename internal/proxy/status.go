package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// The status page's sites on the proxy server: nginx serves each site's
// domain over HTTP (and HTTPS, with Let's Encrypt certificates certbot
// gets there), passing only the status page's paths to hlm through the
// WireGuard tunnel. hlm's own port isn't open to the internet: the tunnel
// reaches it from the proxy server alone, and both nginx and hlm turn away
// everything but the status page on those domains.
//
// For HTTPS, either certbot gets Let's Encrypt certificates on the proxy, or
// Cloudflare sits in front: hlm points each domain at the proxy server with
// a proxied DNS record, installs a Cloudflare Origin CA certificate for
// nginx, and opens the proxy's firewall to Cloudflare's addresses alone.

// The HTTPS options for the status page's sites (Settings.StatusTLS).
const (
	TLSLetsEncrypt = "letsencrypt"
	TLSCloudflare  = "cloudflare"
)

// Cloudflare is what the status page's sites need from Cloudflare (the
// Cloudflare Tunnel service, with its API token).
type Cloudflare interface {
	// ProxyRecord points a hostname at target through Cloudflare's proxy.
	ProxyRecord(ctx context.Context, hostname, target string) error
	// RemoveProxyRecord removes the record ProxyRecord made.
	RemoveProxyRecord(ctx context.Context, hostname string) error
	// OriginCertificate signs a certificate request (PEM) for a hostname.
	OriginCertificate(ctx context.Context, hostname, csr string) (string, time.Time, error)
	// SSLMode is the SSL/TLS mode of the hostname's zone.
	SSLMode(ctx context.Context, hostname string) (string, error)
	// CloudflareIPs are the ranges Cloudflare connects from.
	CloudflareIPs(ctx context.Context) (ipv4, ipv6 []string, err error)
}

// UseCloudflare lets the status page's sites go through Cloudflare.
func (s *Service) UseCloudflare(c Cloudflare) { s.cloudflare = c }

// originDir holds the Origin CA certificates and keys on the proxy.
const originDir = "/etc/nginx/hlm-origin"

// secretCloudflareDomains lists the domains hlm pointed at the proxy
// through Cloudflare, so it can take them away again.
const secretCloudflareDomains = "proxy-status-cloudflare"

// secretCloudflareIPs keeps Cloudflare's address ranges for the firewall.
const secretCloudflareIPs = "proxy-cloudflare-ips"

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
	// Address is the status page on the proxy server's own address
	// ("http://203.0.113.7/status"), when it's served there.
	Address string `json:"address,omitempty"`
	// AddressError says why it's there without the HTTPS asked for.
	AddressError string `json:"addressError,omitempty"`
	// TLS is the HTTPS option used: "", "letsencrypt" or "cloudflare".
	TLS string `json:"tls,omitempty"`
	// HTTPS lists the domains with a certificate, served over HTTPS.
	HTTPS []string `json:"https"`
	// Notes are things to fix that don't stop the sites working.
	Notes []string `json:"notes,omitempty"`
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
        proxy_set_header X-Forwarded-Proto %s;
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

// StatusCert is a domain's certificate on the proxy.
type StatusCert struct {
	Cert, Key string
	// Redirect sends HTTP to HTTPS. Without it, port 80 serves the page
	// too: Cloudflare's edge does the HTTPS, and may ask over HTTP.
	Redirect bool
	// Cloudflare means only Cloudflare reaches nginx, so the scheme it says
	// the visitor used (X-Forwarded-Proto) is passed on.
	Cloudflare bool
}

// letsEncryptCert is where certbot puts a domain's certificate.
func letsEncryptCert(d string) StatusCert {
	return StatusCert{Cert: "/etc/letsencrypt/live/" + d + "/fullchain.pem", Key: "/etc/letsencrypt/live/" + d + "/privkey.pem", Redirect: true}
}

// originCert is where hlm puts a domain's Origin CA certificate.
func originCert(d string) StatusCert {
	return StatusCert{Cert: originDir + "/" + d + ".crt", Key: originDir + "/" + d + ".key", Cloudflare: true}
}

// StatusAddress serves the status page on the proxy server's own address:
// over HTTP on Port, answering Names (its host name and addresses) on port
// 80, where other sites may be, and anything on another port. With TLS, it's
// HTTPS as Name: on 443 (port 80 redirecting there), or on Port.
type StatusAddress struct {
	Port  int
	Names []string
	Name  string
	TLS   *StatusCert
}

// RenderStatus produces the nginx http configuration for the status page's
// sites: each domain on port 80 (answering Let's Encrypt's challenges, and
// redirecting to HTTPS once it has a Let's Encrypt certificate) and, with a
// certificate, on 443; and, with addr, the proxy's own address.
func RenderStatus(domains []string, certs map[string]StatusCert, upstream string, port int, ipv6 bool, addr *StatusAddress) string {
	var b strings.Builder
	b.WriteString("# Managed by Home Lab Manager; changes made here are overwritten.\n")
	b.WriteString("# The status page's sites, passed to hlm through the WireGuard tunnel.\n")
	if len(domains) == 0 && addr == nil {
		return b.String()
	}
	if a, err := netip.ParseAddr(upstream); err == nil && a.Is6() {
		upstream = "[" + upstream + "]"
	}
	to := upstream + ":" + strconv.Itoa(port)
	listen := func(port string, extra string) string {
		l := "    listen " + port + extra + ";\n"
		if ipv6 {
			l += "    listen [::]:" + port + extra + ";\n"
		}
		return l
	}
	for _, d := range domains {
		c, ok := certs[d]
		httpProto := "$scheme"
		if ok && c.Cloudflare {
			httpProto = "$http_x_forwarded_proto"
		}
		fmt.Fprintf(&b, "\n# %s\nserver {\n%s    server_name %s;\n    access_log off;\n", d, listen("80", ""), d)
		fmt.Fprintf(&b, "    location ^~ /.well-known/acme-challenge/ {\n        root %s;\n    }\n", acmeRoot)
		if ok && c.Redirect {
			b.WriteString("    location / {\n        return 301 https://$host$request_uri;\n    }\n}\n")
		} else {
			b.WriteString(fmt.Sprintf(statusLocations, to, httpProto))
			b.WriteString("}\n")
		}
		if ok {
			fmt.Fprintf(&b, "server {\n%s    server_name %s;\n    access_log off;\n", listen("443", " ssl"), d)
			fmt.Fprintf(&b, "    ssl_certificate %s;\n    ssl_certificate_key %s;\n", c.Cert, c.Key)
			b.WriteString(fmt.Sprintf(statusLocations, to, "$scheme"))
			b.WriteString("}\n")
		}
	}
	if addr != nil {
		p := strconv.Itoa(addr.Port)
		// hlm keeps these to the status page, whatever the Host.
		loc := fmt.Sprintf(statusLocations, to, "$scheme")
		loc = strings.Replace(loc, "        proxy_set_header Host $host;\n", "        proxy_set_header Host $host;\n        proxy_set_header "+statuspage.OnlyHeader+" 1;\n", 1)
		names := strings.Join(addr.Names, " ")
		b.WriteString("\n# The proxy server's own address, where players find the game servers\nserver {\n")
		switch {
		case addr.TLS != nil && addr.Port == 80:
			// Other sites share ports 80 and 443: only the proxy's own names,
			// sent to HTTPS on its domain.
			fmt.Fprintf(&b, "%s    server_name %s;\n    access_log off;\n", listen("80", ""), names)
			fmt.Fprintf(&b, "    location / {\n        return 301 https://%s$request_uri;\n    }\n}\n", addr.Name)
			fmt.Fprintf(&b, "server {\n%s    server_name %s;\n    access_log off;\n", listen("443", " ssl"), addr.Name)
			fmt.Fprintf(&b, "    ssl_certificate %s;\n    ssl_certificate_key %s;\n", addr.TLS.Cert, addr.TLS.Key)
		case addr.TLS != nil:
			fmt.Fprintf(&b, "%s    server_name _;\n    access_log off;\n", listen(p, " ssl default_server"))
			fmt.Fprintf(&b, "    ssl_certificate %s;\n    ssl_certificate_key %s;\n", addr.TLS.Cert, addr.TLS.Key)
			// Plain HTTP to this port goes to HTTPS on it.
			fmt.Fprintf(&b, "    error_page 497 =301 https://%s:%s$request_uri;\n", addr.Name, p)
		case addr.Port == 80:
			// Other sites share port 80: only the proxy's own names.
			fmt.Fprintf(&b, "%s    server_name %s;\n    access_log off;\n", listen(p, ""), names)
		default:
			fmt.Fprintf(&b, "%s    server_name _;\n    access_log off;\n", listen(p, " default_server"))
		}
		b.WriteString(loc)
		b.WriteString("}\n")
	}
	return b.String()
}

// statusAddressNames are the names the proxy's own address answers to on
// port 80: its host as hlm knows it and its addresses, IPv6 ones in
// brackets as nginx wants them.
func statusAddressNames(host string, addrs []string) []string {
	var out []string
	add := func(n string) {
		if n = strings.ToLower(strings.Trim(n, "[]")); n == "" {
			return
		}
		if a, err := netip.ParseAddr(n); err == nil {
			if a.IsLoopback() || a.IsLinkLocalUnicast() {
				return
			}
			n = a.String()
			if a.Is6() {
				n = "[" + n + "]"
			}
		} else if !statusDomain.MatchString(n) {
			return
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	add(host)
	for _, a := range addrs {
		add(a)
	}
	return out
}

// statusAddressURL is where visitors find the status page on the proxy's
// own address.
func statusAddressURL(host string, port int) string {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && a.Is6() {
		host = "[" + a.String() + "]"
	}
	if port != 80 {
		host += ":" + strconv.Itoa(port)
	}
	return "http://" + host + "/status"
}

// statusAddressHTTPSURL is where it is over HTTPS, as name.
func statusAddressHTTPSURL(name string, port int) string {
	if port != 80 {
		name += ":" + strconv.Itoa(port)
	}
	return "https://" + name + "/status"
}

// statusPortConflict says which relayed server already has the status
// page's port on the proxy, if one does.
func statusPortConflict(st Settings, servers []Proxied) string {
	if !st.StatusOnAddress {
		return ""
	}
	for _, p := range servers {
		for _, m := range p.Ports {
			if m.Proto == "tcp" && m.Port == st.StatusPort {
				return fmt.Sprintf("port %d/tcp on the proxy already relays %s, so the status page can't have it; choose another port for it", m.Port, p.Server)
			}
		}
	}
	return ""
}

// statusProbeScript says whether nginx's http block reads conf.d, whether
// IPv6 is there, and which of the domains (on stdin, one per line) have a
// Let's Encrypt certificate, and an Origin CA one good for 30 more days.
const statusProbeScript = `PATH="$PATH:/usr/sbin:/sbin"
dump=$(nginx -T 2>/dev/null)
printf '%s\n' "$dump" | grep -qE 'include[[:space:]]+/etc/nginx/conf\.d/\*\.conf' && echo "hlm:confd"
[ -f /proc/net/if_inet6 ] && echo "hlm:ipv6"
ip -o addr show scope global 2>/dev/null | awk '$2 != "gsh0" { split($4, a, "/"); print "hlm:addr:" a[1] }'
command -v certbot >/dev/null 2>&1 && echo "hlm:certbot"
while IFS= read -r d; do
  [ -n "$d" ] || continue
  [ -f "/etc/letsencrypt/live/$d/fullchain.pem" ] && echo "hlm:cert:$d"
  o=` + originDir + `/$d
  if [ -f "$o.crt" ] && [ -f "$o.key" ]; then
    if ! command -v openssl >/dev/null 2>&1 || openssl x509 -checkend 2592000 -noout -in "$o.crt" >/dev/null 2>&1; then
      echo "hlm:origin:$d"
    fi
  fi
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

// originInstallScript installs a domain's Origin CA certificate and key,
// read from stdin as one base64 line each, readable by root alone.
const originInstallScript = `set -e
dir=` + originDir + `
mkdir -p "$dir"; chmod 700 "$dir"
umask 077
IFS= read -r crt; IFS= read -r key
printf '%s' "$crt" | base64 -d > "$dir/$d.crt.new"
printf '%s' "$key" | base64 -d > "$dir/$d.key.new"
mv "$dir/$d.crt.new" "$dir/$d.crt"; mv "$dir/$d.key.new" "$dir/$d.key"
echo "hlm:installed"`

// statusProbe is what statusProbeScript found.
type statusProbe struct {
	confd, ipv6, certbot bool
	addrs                []string        // the proxy server's own addresses
	certs                map[string]bool // Let's Encrypt
	origin               map[string]bool // Cloudflare Origin CA
}

func (s *Service) probeStatus(ctx context.Context, domains []string) (statusProbe, error) {
	res, err := s.run(ctx, statusProbeScript, []byte(strings.Join(domains, "\n")+"\n"), nil)
	if err != nil {
		return statusProbe{}, err
	}
	p := statusProbe{certs: map[string]bool{}, origin: map[string]bool{}}
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch line = strings.TrimSpace(line); {
		case line == "hlm:confd":
			p.confd = true
		case line == "hlm:ipv6":
			p.ipv6 = true
		case line == "hlm:certbot":
			p.certbot = true
		case strings.HasPrefix(line, "hlm:addr:"):
			p.addrs = append(p.addrs, strings.TrimPrefix(line, "hlm:addr:"))
		case strings.HasPrefix(line, "hlm:cert:"):
			p.certs[strings.TrimPrefix(line, "hlm:cert:")] = true
		case strings.HasPrefix(line, "hlm:origin:"):
			p.origin[strings.TrimPrefix(line, "hlm:origin:")] = true
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
// say: their domains to hlm through the WireGuard tunnel, with HTTPS if
// that's on. Without them, it removes the configuration. The caller holds
// applyMu.
func (s *Service) applyStatus(ctx context.Context, st Settings) *StatusSiteResult {
	var served bool
	_, _ = s.secrets.Get(secretStatusServed, &served)
	if !st.StatusPage {
		// Turned off: take the sites off the proxy, once.
		if served {
			if _, err := s.writeStatus(ctx, RenderStatus(nil, nil, "", 0, false, nil)); err != nil {
				s.log.Warn("clear the status page's configuration on the proxy", "err", err)
			} else {
				_ = s.secrets.Delete(secretStatusServed)
			}
		}
		s.forgetCloudflare(ctx, nil)
		s.forgetAddressCert(ctx, "")
		return nil
	}
	if !served {
		_ = s.secrets.Set(secretStatusServed, true)
	}
	res := &StatusSiteResult{Domains: []string{}, HTTPS: []string{}, TLS: st.StatusTLS}
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
		d = strings.ToLower(d)
		if statusDomain.MatchString(d) && !slices.Contains(res.Domains, d) {
			res.Domains = append(res.Domains, d)
		}
	}
	slices.Sort(res.Domains)
	if port == 0 {
		return fail(errors.New("hlm can't tell which port it's reached on at the Docker host"))
	}
	if st.StatusTLS == TLSCloudflare && s.cloudflare == nil {
		return fail(errors.New("hlm can't reach Cloudflare here"))
	}
	probe, err := s.probeStatus(ctx, res.Domains)
	if err != nil {
		return fail(err)
	}
	if !probe.confd && (len(res.Domains) > 0 || st.StatusOnAddress) {
		return fail(errors.New("nginx on the proxy server doesn't read /etc/nginx/conf.d/*.conf in its http block, where hlm puts the status page's sites; add that include to nginx.conf"))
	}
	var addr *StatusAddress
	if st.StatusOnAddress {
		addr = &StatusAddress{Port: st.StatusPort, Names: statusAddressNames(st.Host, probe.addrs), Name: st.StatusAddressName}
		res.Address = statusAddressURL(st.Host, st.StatusPort)
		if st.StatusAddressHTTPS {
			addr.Names = statusAddressNames(st.Host, append(probe.addrs, st.StatusAddressName))
			if slices.Contains(res.Domains, st.StatusAddressName) {
				res.AddressError = st.StatusAddressName + " is also a status page site's domain, which already has its own HTTPS option: use another name here, or leave HTTPS off here"
			} else if c, err := s.ensureAddressCert(ctx, st); err != nil {
				res.AddressError = err.Error() + "; it's served over HTTP until that's fixed"
			} else {
				addr.TLS = &c
				res.Address = statusAddressHTTPSURL(st.StatusAddressName, st.StatusPort)
			}
		}
	}
	certs := map[string]StatusCert{}
	var cloudflared []string
	switch st.StatusTLS {
	case TLSLetsEncrypt:
		for d := range probe.certs {
			certs[d] = letsEncryptCert(d)
		}
	case TLSCloudflare:
		// Each domain's record and certificate first: nginx needs nothing
		// from the internet to get them.
		cloudflared = s.cloudflareSites(ctx, st, res, probe, certs)
	}
	render := func() string { return RenderStatus(res.Domains, certs, st.Upstream, port, probe.ipv6, addr) }
	if res.Output, err = s.writeStatus(ctx, render()); err != nil {
		res.Address = ""
		return fail(err)
	}
	// Domains no longer behind Cloudflare lose hlm's records and
	// certificates, now that nginx no longer uses them; so does a
	// certificate for the proxy's address it no longer uses.
	s.forgetCloudflare(ctx, cloudflared)
	if !st.StatusOnAddress || !st.StatusAddressHTTPS {
		s.forgetAddressCert(ctx, "")
	}
	// Let's Encrypt certificates for the domains without one, now that
	// nginx answers their challenges; then HTTPS for those.
	if st.StatusTLS == TLSLetsEncrypt {
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
				certs[d], got = letsEncryptCert(d), true
			}
		}
		if got {
			if res.Output, err = s.writeStatus(ctx, render()); err != nil {
				return fail(err)
			}
		}
	}
	for _, d := range res.Domains {
		if _, ok := certs[d]; ok {
			res.HTTPS = append(res.HTTPS, d)
		}
	}
	s.log.Info("served the status page's sites on the proxy", "domains", len(res.Domains), "https", len(res.HTTPS), "tls", st.StatusTLS, "address", res.Address)
	return res
}

// cloudflareSites puts each domain behind Cloudflare: a proxied record to
// the proxy server and an Origin CA certificate on it (added to certs). It
// returns the domains with a record, and keeps Cloudflare's addresses for
// the firewall.
func (s *Service) cloudflareSites(ctx context.Context, st Settings, res *StatusSiteResult, probe statusProbe, certs map[string]StatusCert) []string {
	var recorded []string
	modes := map[string]bool{}
	for _, d := range res.Domains {
		if err := s.cloudflare.ProxyRecord(ctx, d, st.Host); err != nil {
			res.setFailed(d, err.Error())
			continue
		}
		recorded = append(recorded, d)
		s.trackCloudflare(d)
		if !probe.origin[d] {
			if err := s.installOriginCert(ctx, d); err != nil {
				res.setFailed(d, "the Origin CA certificate: "+err.Error())
				continue
			}
		}
		certs[d] = originCert(d)
		switch mode, err := s.cloudflare.SSLMode(ctx, d); {
		case err != nil:
			s.log.Warn("read the Cloudflare SSL/TLS mode", "domain", d, "err", err)
		case (mode == "off" || mode == "flexible") && !modes[mode]:
			modes[mode] = true
			res.Notes = append(res.Notes, fmt.Sprintf("%s's Cloudflare SSL/TLS mode is %q, so Cloudflare talks to the proxy server unencrypted; set it to Full (strict) in the Cloudflare dashboard (SSL/TLS → Overview)", d, mode))
		}
	}
	if v4, v6, err := s.cloudflare.CloudflareIPs(ctx); err != nil {
		res.Notes = append(res.Notes, "Cloudflare's addresses couldn't be read, so the proxy's firewall lets anyone reach ports 80 and 443: "+err.Error())
	} else if err := s.secrets.Set(secretCloudflareIPs, linode.Addresses{IPv4: v4, IPv6: v6}); err != nil {
		s.log.Warn("save Cloudflare's addresses", "err", err)
	}
	return recorded
}

// installOriginCert has Cloudflare sign a certificate for a domain, for a
// key made here, and installs both on the proxy server.
func (s *Service) installOriginCert(ctx context.Context, domain string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, key)
	if err != nil {
		return err
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	cert, expires, err := s.cloudflare.OriginCertificate(ctx, domain, string(csr))
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	stdin := base64.StdEncoding.EncodeToString([]byte(cert)) + "\n" + base64.StdEncoding.EncodeToString(keyPEM) + "\n"
	out, err := s.run(ctx, "d="+remote.Quote(domain)+"\n"+originInstallScript, []byte(stdin), nil)
	switch {
	case err != nil:
		return err
	case !strings.Contains(out.Stdout, "hlm:installed"):
		return fmt.Errorf("installing it on the proxy server failed (exit status %d): %s", out.ExitCode, lastLine(out.Output()))
	}
	s.log.Info("installed a Cloudflare Origin CA certificate on the proxy", "domain", domain, "expires", expires)
	return nil
}

// trackCloudflare records that hlm made a Cloudflare record for a domain.
func (s *Service) trackCloudflare(domain string) {
	var had []string
	_, _ = s.secrets.Get(secretCloudflareDomains, &had)
	if !slices.Contains(had, domain) {
		had = append(had, domain)
		slices.Sort(had)
		if err := s.secrets.Set(secretCloudflareDomains, had); err != nil {
			s.log.Warn("save the status page's Cloudflare domains", "err", err)
		}
	}
}

// forgetCloudflare takes hlm's Cloudflare records and Origin CA
// certificates away from the domains it set up that aren't in keep.
func (s *Service) forgetCloudflare(ctx context.Context, keep []string) {
	var had []string
	_, _ = s.secrets.Get(secretCloudflareDomains, &had)
	left := slices.Clone(keep)
	var gone []string
	for _, d := range had {
		if slices.Contains(keep, d) {
			continue
		}
		if s.cloudflare != nil {
			if err := s.cloudflare.RemoveProxyRecord(ctx, d); err != nil {
				s.log.Warn("remove a status page domain's Cloudflare record", "domain", d, "err", err)
				left = append(left, d) // try again next time
				continue
			}
		}
		gone = append(gone, d)
	}
	if len(gone) > 0 {
		script := "rm -f"
		for _, d := range gone {
			script += " " + remote.Quote(originDir+"/"+d+".crt") + " " + remote.Quote(originDir+"/"+d+".key")
		}
		if _, err := s.run(ctx, script, nil, nil); err != nil {
			s.log.Warn("remove Origin CA certificates from the proxy", "err", err)
		}
	}
	slices.Sort(left)
	if len(left) == 0 {
		_ = s.secrets.Delete(secretCloudflareDomains)
		_ = s.secrets.Delete(secretCloudflareIPs)
	} else if !slices.Equal(left, had) {
		_ = s.secrets.Set(secretCloudflareDomains, left)
	}
}

// applyStatusRecorded is applyStatus, keeping its outcome. servers are
// the relayed servers, whose ports the status page can't also have.
func (s *Service) applyStatusRecorded(ctx context.Context, st Settings, servers []Proxied) *StatusSiteResult {
	var res *StatusSiteResult
	if why := statusPortConflict(st, servers); why != "" && st.StatusPage {
		res = &StatusSiteResult{Domains: []string{}, HTTPS: []string{}, TLS: st.StatusTLS, Error: why}
	} else {
		res = s.applyStatus(ctx, st)
	}
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

const statusAddressRuleLabel = rulePrefix + "status-address"

// statusAddressRule opens the status page's port on the proxy's own
// address to everyone.
func statusAddressRule(port int, https bool) linode.Rule {
	ports := strconv.Itoa(port)
	if port == 80 && https {
		ports = "80,443"
	}
	return linode.Rule{
		Action: "ACCEPT", Protocol: "TCP", Ports: ports,
		Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}, IPv6: []string{"::/0"}},
		Label:     statusAddressRuleLabel, Description: "Home Lab Manager: the status page on the proxy's address",
	}
}

// statusRule opens HTTP and HTTPS on the proxy's firewall, for the status
// page's sites: to everyone, or to the given addresses (Cloudflare's).
func statusRule(from *linode.Addresses) linode.Rule {
	if from == nil || len(from.IPv4)+len(from.IPv6) == 0 {
		from = &linode.Addresses{IPv4: []string{"0.0.0.0/0"}, IPv6: []string{"::/0"}}
	}
	return linode.Rule{
		Action: "ACCEPT", Protocol: "TCP", Ports: "80,443",
		Addresses: from,
		Label:     statusRuleLabel, Description: "Home Lab Manager: the status page's sites",
	}
}
