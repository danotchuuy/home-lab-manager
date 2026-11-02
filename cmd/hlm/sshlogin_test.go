package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/api"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// hlmWithKey runs hlm's API with an account whose key is a new ed25519 key,
// and returns hlm's URL and the key's private key file.
func hlmWithKey(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %s", out)
	}
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	svc := auth.New(store, false, log)
	if _, err := svc.CreateAccount("alice", "correct horse battery", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	pub, _ := os.ReadFile(key + ".pub")
	if _, err := svc.AddSSHKey("alice", string(pub), "ci", nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler(api.Options{Auth: svc, Groups: rbac.NewStore(store, log), Token: "admin-token", Log: log}))
	t.Cleanup(srv.Close)
	return srv, key
}

func login(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := sshLoginCommand(append([]string{"-url", srv.URL, "-user", "alice"}, args...), &out, &errs)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func works(t *testing.T, srv *httptest.Server, token string) bool {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/groups", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode == 200
}

func TestSSHLoginWithKeyFile(t *testing.T) {
	srv, key := hlmWithKey(t)
	t.Setenv("SSH_AUTH_SOCK", "")
	token, err := login(t, srv, "-key", key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, auth.SSHTokenPrefix) || !works(t, srv, token) {
		t.Errorf("token %q doesn't work", token)
	}
	// The .pub file's name works too.
	if _, err := login(t, srv, "-key", key+".pub"); err != nil {
		t.Errorf("with the .pub name: %v", err)
	}
}

func TestSSHLoginWithAgent(t *testing.T) {
	srv, key := hlmWithKey(t)
	if _, err := exec.LookPath("ssh-agent"); err != nil {
		t.Skip("no ssh-agent")
	}
	sock := filepath.Join(t.TempDir(), "agent.sock")
	agent := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)
	if out, err := exec.Command("ssh-add", "-q", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %s", out)
	}
	// Only the agent has the private key now.
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	token, err := login(t, srv, "-key", key+".pub")
	if err != nil {
		t.Fatal(err)
	}
	if !works(t, srv, token) {
		t.Error("the agent's token doesn't work")
	}
}

func TestSSHLoginErrors(t *testing.T) {
	srv, key := hlmWithKey(t)
	t.Setenv("SSH_AUTH_SOCK", "")
	var out, errs bytes.Buffer
	if err := sshLoginCommand([]string{"-user", "alice"}, &out, &errs); err == nil {
		t.Error("ran without -url")
	}
	other := filepath.Join(t.TempDir(), "other")
	exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other).Run()
	if _, err := login(t, srv, "-key", other); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("a key hlm doesn't know: %v", err)
	}
	if _, err := login(t, srv, "-key", key+"-missing"); err == nil || !strings.Contains(err.Error(), "ssh-keygen") {
		t.Errorf("a missing key: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q on failure", out.String())
	}
}
