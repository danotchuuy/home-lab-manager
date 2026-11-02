package auth

import (
	"bytes"
	"encoding/pem"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const testRemote = "10.0.3.115:51234"

func sshService(t *testing.T) *Service {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(store, false, slog.New(slog.DiscardHandler))
	if _, err := s.CreateAccount("alice", "correct horse battery", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount("bob", "correct horse battery", rbac.Member); err != nil {
		t.Fatal(err)
	}
	return s
}

// keygen runs the real ssh-keygen, which is what people will use.
type keygen struct{ file string }

func newKeygen(t *testing.T, kind string) keygen {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	file := filepath.Join(t.TempDir(), "key")
	args := []string{"-q", "-t", kind, "-N", "", "-f", file}
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %s", out)
	}
	return keygen{file}
}

func (k keygen) public(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(k.file + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (k keygen) sign(t *testing.T, namespace, message string) string {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", namespace, "-f", k.file)
	cmd.Stdin = strings.NewReader(message)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("sign: %s", errs.String())
	}
	return out.String()
}

func challengeFor(t *testing.T, s *Service, user string) string {
	t.Helper()
	c, _, err := s.SSHChallenge(user)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sshIn(t *testing.T, s *Service, k keygen, user string) string {
	t.Helper()
	c := challengeFor(t, s, user)
	token, _, err := s.SSHLogin(c, k.sign(t, SSHNamespace, c), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestSSHLoginWithEachKeyType(t *testing.T) {
	for _, kind := range []string{"ed25519", "ecdsa", "rsa"} {
		t.Run(kind, func(t *testing.T) {
			s, k := sshService(t), newKeygen(t, kind)
			if _, err := s.AddSSHKey("alice", k.public(t), "", nil); err != nil {
				t.Fatal(err)
			}
			p, ok := s.CheckSSHToken(sshIn(t, s, k, "ALICE"), testRemote)
			if !ok || p.Role != rbac.Admin || p.Name != "ssh:alice" {
				t.Errorf("principal = %+v, %v", p, ok)
			}
			if len(p.Subjects) != 1 || p.Subjects[0] != (rbac.Subject{Kind: rbac.SubjectLocal, Name: "alice"}) {
				t.Errorf("subjects = %+v", p.Subjects)
			}
			keys, _ := s.SSHKeys("alice")
			if len(keys) != 1 || keys[0].LastUsed.IsZero() {
				t.Errorf("keys = %+v", keys)
			}
		})
	}
}

func TestSSHLoginRefused(t *testing.T) {
	s, k, other := sshService(t), newKeygen(t, "ed25519"), newKeygen(t, "ed25519")
	if _, err := s.AddSSHKey("alice", k.public(t), "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSSHKey("bob", other.public(t), "", nil); err != nil {
		t.Fatal(err)
	}
	refused := func(name, challenge, signature string) {
		t.Helper()
		if token, _, err := s.SSHLogin(challenge, signature, testRemote); err == nil || token != "" {
			t.Errorf("%s was let in", name)
		}
	}
	c := challengeFor(t, s, "alice")
	refused("a key on another account", c, other.sign(t, SSHNamespace, c))
	refused("a signature for something else", c, k.sign(t, "git", c))
	refused("a signature of other text", c, k.sign(t, SSHNamespace, c+"x"))
	refused("nothing", c, "")
	refused("not a challenge from hlm", "made-up", k.sign(t, SSHNamespace, "made-up"))
	nobody := challengeFor(t, s, "nobody")
	refused("no such account", nobody, k.sign(t, SSHNamespace, nobody))

	// A token can't stand in for a challenge, or the other way round.
	token := sshIn(t, s, k, "alice")
	raw := strings.TrimPrefix(token, SSHTokenPrefix)
	refused("a token as a challenge", raw, k.sign(t, SSHNamespace, raw))
	if _, ok := s.CheckSSHToken(SSHTokenPrefix+c, testRemote); ok {
		t.Error("a challenge worked as a token")
	}

	// A challenge works once.
	c = challengeFor(t, s, "alice")
	sig := k.sign(t, SSHNamespace, c)
	if _, _, err := s.SSHLogin(c, sig, testRemote); err != nil {
		t.Fatal(err)
	}
	refused("a used challenge", c, sig)

	// A challenge and a token expire.
	c = challengeFor(t, s, "alice")
	sig = k.sign(t, SSHNamespace, c)
	token = sshIn(t, s, k, "alice")
	now := time.Now()
	s.now = func() time.Time { return now.Add(sshChallengeLife + time.Second) }
	refused("an old challenge", c, sig)
	s.now = func() time.Time { return now.Add(sshTokenLife + time.Second) }
	if _, ok := s.CheckSSHToken(token, testRemote); ok {
		t.Error("an old token worked")
	}
}

func TestSHA1SignaturesRefused(t *testing.T) {
	k := newKeygen(t, "rsa")
	sig := k.sign(t, SSHNamespace, "hello")
	block, _ := pem.Decode([]byte(sig))
	body, _ := strings.CutPrefix(string(block.Bytes), sshSigMagic)
	var in sshSignature
	if err := ssh.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	var inner ssh.Signature
	if err := ssh.Unmarshal(in.Signature, &inner); err != nil {
		t.Fatal(err)
	}
	inner.Format = ssh.KeyAlgoRSA
	in.Signature = ssh.Marshal(inner)
	block.Bytes = append([]byte(sshSigMagic), ssh.Marshal(in)...)
	if _, err := verifySSHSignature(string(pem.EncodeToMemory(block)), []byte("hello")); err == nil || !strings.Contains(err.Error(), "SHA-1") {
		t.Errorf("err = %v", err)
	}
}

func TestSSHTokenFollowsTheAccount(t *testing.T) {
	s, k := sshService(t), newKeygen(t, "ed25519")
	key, err := s.AddSSHKey("bob", k.public(t), "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	token := sshIn(t, s, k, "bob")

	// The role is read each time.
	if p, ok := s.CheckSSHToken(token, testRemote); !ok || p.Role != rbac.Member {
		t.Fatalf("principal = %+v, %v", p, ok)
	}
	if _, err := s.SetAccountRole(nil, "bob", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.CheckSSHToken(token, testRemote); p.Role != rbac.Admin {
		t.Errorf("after promotion, role = %s", p.Role)
	}

	// A new password ends it, as it does sessions, but leaves the key.
	if _, err := s.setPassword("bob", "a whole new password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckSSHToken(token, testRemote); ok {
		t.Error("the token outlived the password")
	}

	// Removing the key ends it at once.
	token = sshIn(t, s, k, "bob")
	if err := s.DeleteSSHKey("bob", key.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckSSHToken(token, testRemote); ok {
		t.Error("the token outlived the key")
	}
	c := challengeFor(t, s, "bob")
	if _, _, err := s.SSHLogin(c, k.sign(t, SSHNamespace, c), testRemote); err == nil {
		t.Error("a removed key signed in")
	}

	// So does removing the account.
	if _, err := s.AddSSHKey("bob", k.public(t), "", nil); err != nil {
		t.Fatal(err)
	}
	token = sshIn(t, s, k, "bob")
	if err := s.DeleteAccount(nil, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckSSHToken(token, testRemote); ok {
		t.Error("the token outlived the account")
	}
}

func TestAddSSHKeyChecks(t *testing.T) {
	s, k := sshService(t), newKeygen(t, "ed25519")
	fields := strings.Fields(k.public(t))
	pub := fields[0] + " " + fields[1]
	got, err := s.AddSSHKey("alice", pub+" laptop", "", nil)
	if err != nil || got.Name != "laptop" || got.Type != "ssh-ed25519" || !strings.HasPrefix(got.Fingerprint, "SHA256:") {
		t.Fatalf("key = %+v, %v", got, err)
	}
	for name, line := range map[string]string{
		"duplicate":   pub,
		"nonsense":    "hello there",
		"private key": "-----BEGIN OPENSSH PRIVATE KEY-----",
		"options":     `command="/bin/sh" ` + strings.TrimSpace(newKeygen(t, "ed25519").public(t)),
		"two keys":    strings.TrimSpace(newKeygen(t, "ed25519").public(t)) + "\n" + strings.TrimSpace(newKeygen(t, "ed25519").public(t)),
		"cut short":   fields[0] + " " + fields[1][:20],
	} {
		if _, err := s.AddSSHKey("alice", line, "", nil); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := s.AddSSHKey("nobody", strings.TrimSpace(newKeygen(t, "ed25519").public(t)), "", nil); err != ErrNoAccount {
		t.Errorf("no account: %v", err)
	}
	if _, err := s.AddSSHKey("alice", strings.TrimSpace(newKeygen(t, "ed25519").public(t)), strings.Repeat("x", 101), nil); err == nil {
		t.Error("a long name was accepted")
	}
	weak := filepath.Join(t.TempDir(), "weak")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "rsa", "-b", "1024", "-N", "", "-f", weak).CombinedOutput(); err == nil {
		b, _ := os.ReadFile(weak + ".pub")
		if _, err := s.AddSSHKey("alice", string(b), "", nil); err == nil {
			t.Error("a 1024-bit RSA key was accepted")
		}
	} else {
		t.Logf("no weak key to try: %s", out)
	}
}
