// Package nuttest runs a fake upsd for tests.
package nuttest

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Server is a fake upsd with UPSes whose variables tests can change.
type Server struct {
	Addr string
	// Username and Password, when set, are required before LIST.
	Username, Password string

	mu   sync.Mutex
	upss map[string]map[string]string
	ln   net.Listener
}

// New starts a fake upsd, closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: ln.Addr().String(), upss: map[string]map[string]string{}, ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Set sets a UPS's variables (adding the UPS), keeping the others.
func (s *Server) Set(ups string, vars map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upss[ups] == nil {
		s.upss[ups] = map[string]string{}
	}
	for k, v := range vars {
		s.upss[ups][k] = v
	}
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	var user, pass string
	w := func(f string, a ...any) { fmt.Fprintf(c, f+"\n", a...) }
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		arg := func(i int) string {
			if len(f) <= i {
				return ""
			}
			return strings.Trim(strings.Join(f[i:], " "), `"`)
		}
		switch f[0] {
		case "USERNAME":
			user = arg(1)
			w("OK")
		case "PASSWORD":
			pass = arg(1)
			w("OK")
		case "LOGOUT":
			w("OK Goodbye")
			return
		case "LIST":
			if s.Username != "" && (user != s.Username || pass != s.Password) {
				w("ERR ACCESS-DENIED")
				continue
			}
			s.mu.Lock()
			switch {
			case len(f) == 2 && f[1] == "UPS":
				w("BEGIN LIST UPS")
				for _, name := range sorted(s.upss) {
					w(`UPS %s "%s"`, name, s.upss[name]["device.description"])
				}
				w("END LIST UPS")
			case len(f) == 3 && f[1] == "VAR":
				vars, ok := s.upss[f[2]]
				if !ok {
					w("ERR UNKNOWN-UPS")
					break
				}
				w("BEGIN LIST VAR %s", f[2])
				for _, k := range sorted(vars) {
					w(`VAR %s %s "%s"`, f[2], k, strings.ReplaceAll(vars[k], `"`, `\"`))
				}
				w("END LIST VAR %s", f[2])
			default:
				w("ERR INVALID-ARGUMENT")
			}
			s.mu.Unlock()
		default:
			w("ERR UNKNOWN-COMMAND")
		}
	}
}

func sorted[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
