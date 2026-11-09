package health

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/reach"
)

// Probes are Kubernetes-style probes, with Kubernetes's names and meanings:
// a startup probe holds the others back until it succeeds (and restarts the
// server if it never does), a liveness probe restarts the server when it
// fails, and a readiness probe says whether it's ready for use. hlm runs
// them itself, as the kubelet does, so they work without recreating the
// container and without tools in its image.
type Probes struct {
	Startup   *Probe `json:"startupProbe,omitempty"`
	Liveness  *Probe `json:"livenessProbe,omitempty"`
	Readiness *Probe `json:"readinessProbe,omitempty"`
}

// The probes, by the names the API and the status use.
const (
	ProbeStartup   = "startup"
	ProbeLiveness  = "liveness"
	ProbeReadiness = "readiness"
)

// Empty says there are no probes.
func (p Probes) Empty() bool { return p.Startup == nil && p.Liveness == nil && p.Readiness == nil }

// each calls f with each probe that's set, by name.
func (p Probes) each(f func(name string, pr *Probe)) {
	for _, x := range []struct {
		name string
		pr   *Probe
	}{{ProbeStartup, p.Startup}, {ProbeLiveness, p.Liveness}, {ProbeReadiness, p.Readiness}} {
		if x.pr != nil {
			f(x.name, x.pr)
		}
	}
}

// Probe is one probe: one way to check, and its timings.
type Probe struct {
	Exec      *ExecAction      `json:"exec,omitempty"`
	HTTPGet   *HTTPGetAction   `json:"httpGet,omitempty"`
	TCPSocket *TCPSocketAction `json:"tcpSocket,omitempty"`

	InitialDelaySeconds int `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int `json:"timeoutSeconds,omitempty"`
	SuccessThreshold    int `json:"successThreshold,omitempty"`
	FailureThreshold    int `json:"failureThreshold,omitempty"`
}

// ExecAction runs a command in the container (not in a shell: use
// ["sh", "-c", "…"] for one); exit code 0 is success.
type ExecAction struct {
	Command []string `json:"command"`
}

// HTTPGetAction asks for a page; 200 to 399 is success. Redirects aren't
// followed, and HTTPS certificates aren't checked, as in Kubernetes.
type HTTPGetAction struct {
	Path        string       `json:"path,omitempty"`
	Port        Port         `json:"port"`
	Host        string       `json:"host,omitempty"`
	Scheme      string       `json:"scheme,omitempty"` // HTTP or HTTPS
	HTTPHeaders []HTTPHeader `json:"httpHeaders,omitempty"`
}

// HTTPHeader is a header an HTTP probe sends.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TCPSocketAction connects to a port; connecting is success.
type TCPSocketAction struct {
	Port Port   `json:"port"`
	Host string `json:"host,omitempty"`
}

// Port is a container port. Kubernetes also takes a port's name; hlm takes
// a number, written as a number or a string.
type Port int

// UnmarshalJSON reads 8080 or "8080".
func (p *Port) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*p = Port(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a probe's port is a number")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("a probe's port is a number: hlm doesn't know named ports like %q", s)
	}
	*p = Port(n)
	return nil
}

// Kubernetes's defaults.
const (
	DefaultProbePeriod  = 10 // seconds
	DefaultProbeTimeout = 1  // seconds
	DefaultProbeSuccess = 1
	DefaultProbeFailure = 3
)

// validate checks the probes and fills in the defaults, as Kubernetes's API
// server does.
func (p Probes) validate() (Probes, error) {
	var err error
	out := Probes{}
	for _, x := range []struct {
		name string
		in   *Probe
		out  **Probe
	}{{ProbeStartup, p.Startup, &out.Startup}, {ProbeLiveness, p.Liveness, &out.Liveness}, {ProbeReadiness, p.Readiness, &out.Readiness}} {
		if x.in == nil {
			continue
		}
		pr := *x.in
		if pr, err = pr.validate(x.name); err != nil {
			return p, fmt.Errorf("%sProbe: %w", x.name, err)
		}
		*x.out = &pr
	}
	return out, nil
}

func (pr Probe) validate(name string) (Probe, error) {
	n := 0
	if pr.Exec != nil {
		n++
		e := *pr.Exec
		e.Command = append([]string(nil), e.Command...)
		if len(e.Command) == 0 || strings.TrimSpace(e.Command[0]) == "" {
			return pr, errors.New("exec needs a command")
		}
		size := 0
		for _, a := range e.Command {
			size += len(a)
		}
		if size > 4000 {
			return pr, errors.New("the command is too long")
		}
		pr.Exec = &e
	}
	if pr.HTTPGet != nil {
		n++
		h := *pr.HTTPGet
		if h.Port < 1 || h.Port > 65535 {
			return pr, errors.New("httpGet needs the port, as the server listens on it in its container")
		}
		h.Path = strings.TrimSpace(h.Path)
		if h.Path == "" {
			h.Path = "/"
		}
		if !strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, " \r\n") {
			return pr, errors.New("httpGet's path starts with / and has no spaces")
		}
		h.Scheme = strings.ToUpper(strings.TrimSpace(h.Scheme))
		switch h.Scheme {
		case "":
			h.Scheme = "HTTP"
		case "HTTP", "HTTPS":
		default:
			return pr, errors.New("httpGet's scheme is HTTP or HTTPS")
		}
		if err := validHost(h.Host); err != nil {
			return pr, err
		}
		h.HTTPHeaders = append([]HTTPHeader(nil), h.HTTPHeaders...)
		for _, hd := range h.HTTPHeaders {
			if strings.TrimSpace(hd.Name) == "" || strings.ContainsAny(hd.Name+hd.Value, "\r\n") || strings.ContainsAny(hd.Name, " :") {
				return pr, fmt.Errorf("httpGet's header %q isn't one", hd.Name)
			}
		}
		pr.HTTPGet = &h
	}
	if pr.TCPSocket != nil {
		n++
		t := *pr.TCPSocket
		if t.Port < 1 || t.Port > 65535 {
			return pr, errors.New("tcpSocket needs the port, as the server listens on it in its container")
		}
		if err := validHost(t.Host); err != nil {
			return pr, err
		}
		pr.TCPSocket = &t
	}
	if n != 1 {
		return pr, errors.New("give exactly one of exec, httpGet and tcpSocket")
	}
	for _, r := range []struct {
		field    string
		v        *int
		def      int
		min, max int
	}{
		{"initialDelaySeconds", &pr.InitialDelaySeconds, 0, 0, 3600},
		{"periodSeconds", &pr.PeriodSeconds, DefaultProbePeriod, 1, 3600},
		{"timeoutSeconds", &pr.TimeoutSeconds, DefaultProbeTimeout, 1, 300},
		{"successThreshold", &pr.SuccessThreshold, DefaultProbeSuccess, 1, 100},
		{"failureThreshold", &pr.FailureThreshold, DefaultProbeFailure, 1, 1000},
	} {
		if *r.v == 0 {
			*r.v = r.def
		}
		if *r.v < r.min || *r.v > r.max {
			return pr, fmt.Errorf("%s is %d to %d", r.field, r.min, r.max)
		}
	}
	if name != ProbeReadiness && pr.SuccessThreshold != 1 {
		return pr, errors.New("successThreshold must be 1 for liveness and startup probes")
	}
	return pr, nil
}

func validHost(h string) error {
	if h != "" && (strings.ContainsAny(h, " /\r\n") || len(h) > 253) {
		return fmt.Errorf("%q isn't a host name or address", h)
	}
	return nil
}

// ProbeEnv is what running a probe needs.
type ProbeEnv struct {
	// HostAddresses are the addresses of the server's Docker host, for
	// ports it publishes.
	HostAddresses []string
	// Exec runs a command in the container and returns its exit code.
	Exec func(ctx context.Context, id string, hc docker.Healthcheck) (int, string, error)
	// Dial is for tests.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Run runs the probe once against a running container: whether it
// succeeded, and what it saw.
func (pr Probe) Run(ctx context.Context, info docker.ContainerJSON, env ProbeEnv) (bool, string) {
	timeout := time.Duration(max(pr.TimeoutSeconds, 1)) * time.Second
	switch {
	case pr.Exec != nil:
		if env.Exec == nil {
			return false, "commands can't be run here"
		}
		code, out, err := env.Exec(ctx, info.ID, docker.Healthcheck{
			Test: append([]string{"CMD"}, pr.Exec.Command...), Timeout: int64(timeout),
		})
		if err != nil {
			return false, err.Error()
		}
		msg := fmt.Sprintf("exit code %d", code)
		if out != "" {
			msg += ": " + firstLine(out)
		}
		return code == 0, msg
	case pr.TCPSocket != nil:
		addrs := probeAddrs(info, int(pr.TCPSocket.Port), pr.TCPSocket.Host, env.HostAddresses)
		if len(addrs) == 0 {
			return false, unreachable
		}
		var errs []string
		for _, a := range addrs {
			c, err := dial(ctx, env, timeout, a)
			if err == nil {
				c.Close()
				return true, "connected to " + a
			}
			errs = append(errs, a+": "+shortErr(err))
		}
		return false, strings.Join(errs, "; ")
	case pr.HTTPGet != nil:
		return pr.HTTPGet.run(ctx, info, env, timeout)
	}
	return false, "no probe"
}

const unreachable = "hlm can't reach this server: it has no address on a network hlm can use, and the port isn't published"

func (h HTTPGetAction) run(ctx context.Context, info docker.ContainerJSON, env ProbeEnv, timeout time.Duration) (bool, string) {
	addrs := probeAddrs(info, int(h.Port), h.Host, env.HostAddresses)
	if len(addrs) == 0 {
		return false, unreachable
	}
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // as the kubelet: a probe isn't about the certificate
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dial(ctx, env, timeout, addr)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var errs []string
	for _, a := range addrs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.ToLower(h.Scheme)+"://"+a+h.Path, nil)
		if err != nil {
			return false, err.Error()
		}
		req.Header.Set("User-Agent", "hlm-probe")
		for _, hd := range h.HTTPHeaders {
			if strings.EqualFold(hd.Name, "Host") {
				req.Host = hd.Value
			} else {
				req.Header.Add(hd.Name, hd.Value)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, a+": "+shortErr(err))
			continue
		}
		var body [256]byte
		n, _ := resp.Body.Read(body[:])
		resp.Body.Close()
		msg := fmt.Sprintf("HTTP %d from %s", resp.StatusCode, a)
		ok := resp.StatusCode >= 200 && resp.StatusCode < 400
		if !ok {
			if b := firstLine(string(bytes.TrimSpace(body[:n]))); b != "" {
				msg += ": " + b
			}
		}
		return ok, msg // it answered: other addresses won't say otherwise
	}
	return false, strings.Join(errs, "; ")
}

// probeAddrs are where to reach a container port: the probe's host, or the
// container itself (each of its addresses hlm can reach, or the port
// published on its Docker host).
func probeAddrs(info docker.ContainerJSON, port int, host string, hostAddrs []string) []string {
	if host != "" {
		return []string{net.JoinHostPort(host, strconv.Itoa(port))}
	}
	return reach.Addresses(info, port, "tcp", hostAddrs)
}

func dial(ctx context.Context, env ProbeEnv, timeout time.Duration, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if env.Dial != nil {
		return env.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func shortErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "Timeout exceeded"):
		return "no answer"
	case strings.Contains(msg, "connection refused"):
		return "nothing is listening on that port"
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return "can't be reached"
	}
	return msg
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Redacted is the probes without what could be a secret (commands and
// headers' values), for people who may only look.
func (p Probes) Redacted() Probes {
	out := Probes{}
	for _, x := range []struct {
		in  *Probe
		out **Probe
	}{{p.Startup, &out.Startup}, {p.Liveness, &out.Liveness}, {p.Readiness, &out.Readiness}} {
		if x.in == nil {
			continue
		}
		pr := *x.in
		if pr.Exec != nil {
			pr.Exec = &ExecAction{Command: []string{"…"}}
		}
		if h := pr.HTTPGet; h != nil {
			c := *h
			c.HTTPHeaders = nil
			for _, hd := range h.HTTPHeaders {
				c.HTTPHeaders = append(c.HTTPHeaders, HTTPHeader{Name: hd.Name, Value: "…"})
			}
			pr.HTTPGet = &c
		}
		*x.out = &pr
	}
	return out
}
