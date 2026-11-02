package main

// hlm ssh-login signs in to hlm's API with an SSH key and prints a token that
// lasts an hour, so automation needn't hold a password or long-lived key, nor
// reimplement the challenge. ssh-keygen does the signing: with the agent at
// SSH_AUTH_SOCK if there is one, else with the private key file.

import (
	"bytes"
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
)

const sshLoginUsage = `Usage: hlm ssh-login -url URL -user USER [-key FILE] [-ca FILE]

Signs in to hlm with an SSH key and prints a token for the API's
Authorization header. The token lasts an hour.

  -url   hlm's address, e.g. http://192.168.11.101:8080 (or $HLM_URL)
  -user  the account the key belongs to (or $HLM_USER)
  -key   the key's .pub file, or its private key file (default ~/.ssh/id_ed25519)
  -ca    a CA certificate to trust for hlm's HTTPS certificate

With SSH_AUTH_SOCK set, the agent signs, so only the .pub file is needed.

Example:
  token=$(hlm ssh-login -url http://hlm:8080 -user alice)
  curl -H "Authorization: Bearer $token" http://hlm:8080/api/servers
`

func sshLoginCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ssh-login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { io.WriteString(stderr, sshLoginUsage) }
	home, _ := os.UserHomeDir()
	base := fs.String("url", os.Getenv("HLM_URL"), "")
	user := fs.String("user", os.Getenv("HLM_USER"), "")
	key := fs.String("key", filepath.Join(home, ".ssh", "id_ed25519"), "")
	ca := fs.String("ca", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" || *user == "" {
		fs.Usage()
		return errors.New("give -url and -user")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("%s has no PEM certificates", *ca)
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	api := strings.TrimSuffix(*base, "/") + "/api/auth/ssh/"

	var challenge struct {
		Challenge string `json:"challenge"`
		Namespace string `json:"namespace"`
	}
	if err := postJSON(client, api+"challenge", map[string]string{"username": *user}, &challenge); err != nil {
		return fmt.Errorf("get a challenge: %w", err)
	}
	if challenge.Namespace != auth.SSHNamespace {
		return fmt.Errorf("hlm asked for a signature for %q, not %q", challenge.Namespace, auth.SSHNamespace)
	}
	signature, err := sshSign(*key, challenge.Challenge, stderr)
	if err != nil {
		return err
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := postJSON(client, api+"login", map[string]string{"challenge": challenge.Challenge, "signature": signature}, &login); err != nil {
		return fmt.Errorf("sign in: %w", err)
	}
	_, err = fmt.Fprintln(stdout, login.Token)
	return err
}

// sshSign signs message with ssh-keygen: with the agent if there is one, so
// the private key needn't be on disk, else with the private key file.
func sshSign(key, message string, stderr io.Writer) (string, error) {
	private := strings.TrimSuffix(key, ".pub")
	args := []string{"-Y", "sign", "-n", auth.SSHNamespace, "-f", private}
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		args = []string{"-Y", "sign", "-n", auth.SSHNamespace, "-U", "-f", private + ".pub"}
	}
	cmd := exec.Command("ssh-keygen", args...)
	cmd.Stdin = strings.NewReader(message)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(strings.ReplaceAll(errs.String(), "Signing data on standard input\n", ""))
		return "", fmt.Errorf("ssh-keygen couldn't sign: %s", cmp.Or(msg, err.Error()))
	}
	return out.String(), nil
}

func postJSON(client *http.Client, url string, body, out any) error {
	b, _ := json.Marshal(body)
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}
