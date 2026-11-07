package hosts

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// InstallAgent signs in, starts the agent with a new token (here, the real
// agent in this process, when the fake host is asked to "docker run" it)
// and waits for it to connect.
func TestInstallAgent(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	kp, _ := remote.GenerateKey("hlm@test")
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.Public))
	store, _ := secrets.Open(t.TempDir(), "")
	hub := agent.NewHub(log)
	defer hub.Close()
	r := New(Options{Secrets: store, Agents: hub, Log: log,
		SSHKey: func() (remote.KeyPair, bool, error) { return kp, true, nil }})

	mux := http.NewServeMux()
	mux.Handle("GET /api/agent/connect", hub)
	mux.HandleFunc("POST /api/agent/enrol", func(w http.ResponseWriter, req *http.Request) {
		var in agent.EnrolRequest
		json.NewDecoder(req.Body).Decode(&in)
		h, cred, err := r.Enrol(in.Token)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(agent.EnrolResponse{Host: h.ID, Credential: cred})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	probe := []string{"id=debian", "uid=1000", "user=pi", "docker=yes", "socket=yes", "systemd=yes", "sudo=yes"}
	var commands []string
	agentDone := make(chan error, 1)
	target, hostKey, _ := sshServer(t, pub, sshOptions{exec: func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		mu.Lock()
		commands = append(commands, cmd)
		lines := probe
		mu.Unlock()
		switch {
		case strings.Contains(cmd, "/etc/os-release"):
			io.WriteString(stdout, strings.Join(lines, "\n")+"\n")
		case strings.Contains(cmd, "docker run"):
			token, _ := bufio.NewReader(stdin).ReadString('\n')
			io.WriteString(stdout, "Starting the agent\n")
			go func() {
				agentDone <- agent.Run(ctx, agent.Config{Server: srv.URL, Token: strings.TrimSpace(token), DataDir: t.TempDir(),
					Socket: filepath.Join(t.TempDir(), "none.sock"), AllowHTTP: true, Log: log})
			}()
		default:
			io.WriteString(stderr, "unexpected\n")
			return 1
		}
		return 0
	}})
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	in := AgentInput{Input: Input{Host: Host{ID: "pi", Name: "Pi", AppdataRoot: "/srv/appdata", SSH: &target}}, Server: srv.URL + "/"}

	var out []string
	h, err := r.InstallAgent(ctx, in, func(l string) { out = append(out, l) })
	if err != nil {
		t.Fatalf("install: %v\n%s", err, strings.Join(out, "\n"))
	}
	if h.ID != "pi" || h.Kind != KindAgent || h.Address != "127.0.0.1" || h.SSH != nil || !hub.Status("pi").Connected {
		t.Fatalf("host %+v, connected %v", h, hub.Status("pi").Connected)
	}
	mu.Lock()
	run := commands[len(commands)-1]
	mu.Unlock()
	// Not in the docker group, but with sudo; the token isn't on the
	// command line, where sudo would log it.
	if !strings.HasPrefix(run, "sudo -n sh -c ") || strings.Contains(run, "hlme_") || !strings.Contains(run, "--allow-http") ||
		!strings.Contains(run, srv.URL+"'") {
		t.Errorf("command: %s", run)
	}

	// Already there.
	if _, err := r.InstallAgent(ctx, in, func(string) {}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("again: %v", err)
	}
	in.ID = "pi2"
	for _, tt := range []struct {
		probe  []string
		server string
		want   string
	}{
		{[]string{"id=debian", "uid=1000", "user=pi", "sudo=yes"}, srv.URL, "isn't installed and running"},
		{[]string{"id=debian", "uid=1000", "user=pi", "docker=yes", "socket=yes"}, srv.URL, "can't use Docker"},
		{probe, "ftp://hlm.lan", "hlm's address"},
		{probe, "hlm.lan", "hlm's address"},
	} {
		mu.Lock()
		probe, commands = tt.probe, nil
		mu.Unlock()
		in.Server = tt.server
		if _, err := r.InstallAgent(ctx, in, func(string) {}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v %s: %v", tt.probe, tt.server, err)
		}
		mu.Lock()
		for _, c := range commands {
			if strings.Contains(c, "docker run") {
				t.Errorf("%v: started the agent", tt.probe)
			}
		}
		mu.Unlock()
	}
	cancel()
	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Error("agent didn't stop")
	}
}

// The script, run with a fake docker command: it starts the agent with the
// token from stdin, replaces an earlier agent for the same hlm, and leaves
// one for another hlm alone.
func TestAgentScript(t *testing.T) {
	bin := t.TempDir()
	fake := `#!/bin/sh
echo "$@" >> "$DOCKER_LOG"
if [ "$1 $2" = "container inspect" ]; then
  [ -n "$EXISTING" ] || exit 1
  [ "$3" = "-f" ] && printf 'agent\n--server\n%s\n--token\nold\n' "$EXISTING"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, server, existing string
		allowHTTP, fails       bool
		want, not              []string
	}{
		{"new", "https://hlm.example.com", "", false, false,
			[]string{"pull -q " + AgentImage, "run -d --name hlm-agent", "agent --server https://hlm.example.com --token hlme_tok"}, []string{"rm -f", "--allow-http"}},
		{"over http", "http://192.168.1.5:8080", "", true, false, []string{"--server http://192.168.1.5:8080 --allow-http --token hlme_tok"}, nil},
		{"replaces its own", "https://hlm.example.com", "https://hlm.example.com", false, false, []string{"rm -f hlm-agent", "run -d"}, nil},
		{"another hlm's", "https://hlm.example.com", "https://other.example.com", false, true, nil, []string{"rm -f", "run -d", "pull"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logf := filepath.Join(t.TempDir(), "log")
			cmd := exec.Command("sh", "-c", agentScript(tt.server, tt.allowHTTP))
			cmd.Stdin = strings.NewReader("hlme_tok\n")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DOCKER_LOG="+logf, "EXISTING="+tt.existing)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tt.fails {
				t.Fatalf("err %v\n%s", err, out)
			}
			if tt.fails && !strings.Contains(string(out), "other.example.com") {
				t.Errorf("output: %s", out)
			}
			got, _ := os.ReadFile(logf)
			for _, w := range tt.want {
				if !strings.Contains(string(got), w) {
					t.Errorf("no %q in\n%s", w, got)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(string(got), n) {
					t.Errorf("%q in\n%s", n, got)
				}
			}
		})
	}
}
