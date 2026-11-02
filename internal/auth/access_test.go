package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

func TestParseAllowFrom(t *testing.T) {
	got, err := parseAllowFrom([]string{" 10.0.3.115 ", "10.0.3.0/24", "10.0.3.9/24", "::ffff:192.168.1.5", "fd00::1/8", ""})
	want := []string{"10.0.3.115/32", "10.0.3.0/24", "192.168.1.5/32", "fd00::/8"}
	if err != nil || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := parseAllowFrom(nil); got != nil || err != nil {
		t.Errorf("empty = %v, %v", got, err)
	}
	for _, bad := range []string{"10.0.3", "host.example.com", "10.0.3.0/33", "*"} {
		if _, err := parseAllowFrom([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for remote, want := range map[string]bool{
		"10.0.3.115:5000": true, "10.0.3.7:1": true, "[::ffff:10.0.3.7]:1": true, "192.168.1.5:1": true,
		"10.0.4.1:1": false, "[fe80::1]:1": false, "nonsense": false, "": false,
	} {
		if allowedFrom([]string{"10.0.3.0/24", "192.168.1.5/32"}, remote) != want {
			t.Errorf("%s: want %v", remote, want)
		}
	}
	if !allowedFrom(nil, "anything") {
		t.Error("an empty list should allow anywhere")
	}
}

func TestAPIKeyAllowFrom(t *testing.T) {
	s := sshService(t)
	key, info, err := s.CreateAPIKey("ci", 0, rbac.Member, []string{"10.0.3.115"})
	if err != nil || len(info.AllowFrom) != 1 {
		t.Fatalf("create = %+v, %v", info, err)
	}
	if _, ok := s.CheckAPIKey(key, "10.0.3.115:40000"); !ok {
		t.Error("refused from its address")
	}
	if _, ok := s.CheckAPIKey(key, "10.0.3.116:40000"); ok {
		t.Error("allowed from another address")
	}
	if _, err := s.SetAPIKeyAllowFrom(info.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckAPIKey(key, "10.0.3.116:40000"); !ok {
		t.Error("still limited after clearing")
	}
	if _, _, err := s.CreateAPIKey("bad", 0, rbac.Member, []string{"nope"}); err == nil {
		t.Error("a bad address was accepted")
	}
}

func TestSSHKeyAllowFrom(t *testing.T) {
	s, k := sshService(t), newKeygen(t, "ed25519")
	key, err := s.AddSSHKey("alice", k.public(t), "ci", []string{"10.0.3.115/32"})
	if err != nil {
		t.Fatal(err)
	}
	try := func(remote string) (string, error) {
		c := challengeFor(t, s, "alice")
		token, _, err := s.SSHLogin(c, k.sign(t, SSHNamespace, c), remote)
		return token, err
	}
	if _, err := try("10.0.3.200:1"); err == nil {
		t.Error("signed in from another address")
	}
	token, err := try(testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckSSHToken(token, testRemote); !ok {
		t.Error("token refused from its address")
	}
	// The token is checked on every request, not only at sign-in.
	if _, ok := s.CheckSSHToken(token, "10.0.3.200:1"); ok {
		t.Error("token used from another address")
	}
	if _, err := s.SetSSHKeyAllowFrom("alice", key.ID, []string{"10.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckSSHToken(token, testRemote); ok {
		t.Error("token outlived a change to where its key may be used")
	}
}

func TestSSHLoginRateLimit(t *testing.T) {
	s, k, other := sshService(t), newKeygen(t, "ed25519"), newKeygen(t, "ed25519")
	if _, err := s.AddSSHKey("alice", k.public(t), "", nil); err != nil {
		t.Fatal(err)
	}
	attempt := func(signer keygen, remote string) error {
		c := challengeFor(t, s, "alice")
		_, _, err := s.SSHLogin(c, signer.sign(t, SSHNamespace, c), remote)
		return err
	}
	var locked bool
	for range freeFailures + 1 {
		err := attempt(other, testRemote)
		var le *LoginError
		if asLogin(err, &le) && le.RetryAfter > 0 {
			locked = true
		}
	}
	if !locked {
		t.Fatal("never locked")
	}
	if err := attempt(k, testRemote); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("right key while locked: %v", err)
	}
	// Another address isn't locked out by these failures.
	if err := attempt(k, "10.0.3.116:1"); err != nil {
		t.Errorf("from elsewhere: %v", err)
	}
}

func asLogin(err error, le **LoginError) bool {
	l, ok := err.(*LoginError)
	if ok {
		*le = l
	}
	return ok
}

// A restarted hlm doesn't remember which challenges were used, so it
// doesn't take challenges made before it started.
func TestSSHChallengeFromBeforeRestart(t *testing.T) {
	s, k := sshService(t), newKeygen(t, "ed25519")
	if _, err := s.AddSSHKey("alice", k.public(t), "", nil); err != nil {
		t.Fatal(err)
	}
	c := challengeFor(t, s, "alice")
	sig := k.sign(t, SSHNamespace, c)
	if _, _, err := s.SSHLogin(c, sig, testRemote); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.secrets, false, s.log)
	if _, _, err := restarted.SSHLogin(c, sig, testRemote); err == nil {
		t.Error("a challenge was replayed after a restart")
	}
	c = challengeFor(t, restarted, "alice")
	if _, _, err := restarted.SSHLogin(c, k.sign(t, SSHNamespace, c), testRemote); err != nil {
		t.Errorf("a new challenge after the restart: %v", err)
	}
}

func TestConfirmWindow(t *testing.T) {
	s := sshService(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	if s.Confirmed(User{}) {
		t.Error("never confirmed")
	}
	u := User{Confirmed: now.Add(-ConfirmWindow + time.Second)}
	if !s.Confirmed(u) {
		t.Error("within the window")
	}
	u.Confirmed = now.Add(-ConfirmWindow)
	if s.Confirmed(u) {
		t.Error("after the window")
	}
	u.Confirmed = now.Add(time.Hour)
	if s.Confirmed(u) {
		t.Error("from the future")
	}
}

func TestTickets(t *testing.T) {
	s := sshService(t)
	who := rbac.Principal{Name: "key:ci", Role: rbac.Admin}
	const target = "/api/system/backup/download?key=a"
	issue := func() string {
		id, err := s.IssueTicket(who, target, testRemote)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := issue()
	if p, ok := s.RedeemTicket(id, target, "10.0.3.115:60000"); !ok || p.Name != who.Name {
		t.Errorf("redeem = %+v, %v", p, ok)
	}
	if _, ok := s.RedeemTicket(id, target, testRemote); ok {
		t.Error("used twice")
	}
	if _, ok := s.RedeemTicket(issue(), "/api/system/backup/download?key=b", testRemote); ok {
		t.Error("used for another URL")
	}
	if _, ok := s.RedeemTicket(issue(), target, "10.0.3.116:1"); ok {
		t.Error("used from another address")
	}
	id = issue()
	now := time.Now()
	s.now = func() time.Time { return now.Add(ticketLife + time.Second) }
	if _, ok := s.RedeemTicket(id, target, testRemote); ok {
		t.Error("used after it expired")
	}
}
