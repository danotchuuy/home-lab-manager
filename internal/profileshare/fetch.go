// Package profileshare imports game profiles from links and indexes, keeps
// track of where they came from, and finds updates to them. Updates are
// only offered: a changed profile changes which commands hlm sends, so a
// person looks at the difference first.
package profileshare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// Limits on fetching.
const (
	fetchTimeout = 10 * time.Second
	maxRedirects = 3
	// MaxIndexSize is the largest index read.
	MaxIndexSize = 256 << 10
)

// blocked are ranges hlm never fetches from: loopback, link-local (where
// cloud metadata services live), and the like. Home LAN addresses are
// fine: a Gitea on the LAN is a good place for profiles.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba's metadata service
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fd00:ec2::/32"), // AWS's metadata service
}

// Allowed says whether hlm may connect to an address.
func Allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// ErrBlockedAddress is a link to an address hlm doesn't fetch from.
var ErrBlockedAddress = errors.New("hlm doesn't fetch from this machine's own or link-local addresses")

// Fetcher fetches profiles and indexes over HTTPS.
type Fetcher struct {
	// Allow says which addresses may be connected to (Allowed); tests
	// allow their own server.
	Allow func(netip.Addr) bool
	// Transport is the base transport; nil is a fresh one.
	Transport *http.Transport
	UserAgent string
}

func (f *Fetcher) client() *http.Client {
	allow := f.Allow
	if allow == nil {
		allow = Allowed
	}
	var tr *http.Transport
	if f.Transport != nil {
		tr = f.Transport.Clone()
	} else {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	tr.Proxy = nil // the address check has to see where it really connects
	dialer := &net.Dialer{
		Timeout: fetchTimeout,
		// Checked on the address actually connected to, after DNS, so a
		// name that resolves to a blocked address is refused too.
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !allow(ap.Addr()) {
				return ErrBlockedAddress
			}
			return nil
		},
	}
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Transport: tr,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "https" {
				return errors.New("the link redirects to a page that isn't HTTPS")
			}
			return nil
		},
	}
}

var (
	githubBlobRE = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/blob/(.+)$`)
	gistRE       = regexp.MustCompile(`^https://gist\.github\.com/([^/]+)/([0-9a-f]+)/?$`)
)

// Normalize checks a link and turns GitHub pages into their raw files.
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q isn't a link", raw)
	}
	if u.Scheme != "https" {
		return "", errors.New("links have to be HTTPS")
	}
	if u.User != nil {
		return "", errors.New("links can't have a user name or password in them")
	}
	if len(raw) > 2000 {
		return "", errors.New("that link is too long")
	}
	u.Fragment = ""
	s := u.String()
	if m := githubBlobRE.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", m[1], m[2], m[3]), nil
	}
	if m := gistRE.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("https://gist.githubusercontent.com/%s/%s/raw", m[1], m[2]), nil
	}
	return s, nil
}

// get fetches a link, at most limit bytes.
func (f *Fetcher) get(ctx context.Context, link string, limit int64) ([]byte, error) {
	link, err := Normalize(link)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "text/yaml, text/plain, */*")
	resp, err := f.client().Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return nil, ErrBlockedAddress
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("fetch %s: %w", link, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", link, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", link, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d KiB", link, limit>>10)
	}
	return data, nil
}

// Profile fetches a profile and checks it parses and validates.
func (f *Fetcher) Profile(ctx context.Context, link string) ([]byte, *gameprofile.Profile, error) {
	data, err := f.get(ctx, link, gameprofile.MaxSize)
	if err != nil {
		return nil, nil, err
	}
	p, err := gameprofile.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("that isn't a game profile hlm can use: %w", err)
	}
	return data, p, nil
}

// resolve takes a link relative to base, and checks it.
func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("%q isn't a link", ref)
	}
	return Normalize(b.ResolveReference(r).String())
}
