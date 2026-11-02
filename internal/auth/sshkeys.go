package auth

// SSH keys let a script sign in as a local account without holding a password
// or a long-lived secret. The account keeps public keys. To sign in, the script
// asks for a challenge, signs it with `ssh-keygen -Y sign` (so ssh-agent and
// hardware keys work) and swaps the signature for a short-lived token.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

const (
	// SSHNamespace is what signatures for hlm are made for. ssh-keygen mixes
	// it into what's signed, so a signature made for something else, like a
	// git commit, can't be used here.
	SSHNamespace = "hlm-login"
	// SSHTokenPrefix starts every token from an SSH sign-in.
	SSHTokenPrefix = "hlmssh_"

	sshChallengeLife = 2 * time.Minute
	sshTokenLife     = time.Hour
	maxSSHKeys       = 20
	minRSABits       = 2048
)

// SSHKey is a public key that can sign in as an account.
type SSHKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Fingerprint string    `json:"fingerprint"`
	Key         string    `json:"key"` // in authorized_keys form, without a comment
	Added       time.Time `json:"added"`
	LastUsed    time.Time `json:"lastUsed,omitzero"`
}

// Errors from SSH keys.
var (
	// ErrBadSSHLogin doesn't say what was wrong, so it can't be used to find
	// out which accounts or keys exist.
	ErrBadSSHLogin = errors.New("that isn't a current signature of the challenge by a key on this account")
	ErrNoSSHKey    = errors.New("this account has no SSH key with that ID")
)

// parseSSHKey checks a public key, as in a .pub file, and makes it an SSHKey
// called name, or its comment if name is empty.
func parseSSHKey(line, name string) (SSHKey, error) {
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return SSHKey{}, errors.New("that isn't an SSH public key: paste the contents of a .pub file")
	}
	if len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return SSHKey{}, errors.New("give one key, without authorized_keys options")
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
	case ssh.KeyAlgoRSA:
		c, ok := pub.(ssh.CryptoPublicKey)
		if k, isRSA := c.CryptoPublicKey().(*rsa.PublicKey); !ok || !isRSA || k.N.BitLen() < minRSABits {
			return SSHKey{}, fmt.Errorf("an RSA key needs at least %d bits", minRSABits)
		}
	default:
		return SSHKey{}, fmt.Errorf("%s keys aren't supported: use ed25519, ECDSA or RSA", pub.Type())
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = comment
	}
	if name == "" {
		name = pub.Type()
	}
	if len(name) > 100 {
		return SSHKey{}, errors.New("give the key a name of up to 100 characters")
	}
	sum := sha256.Sum256(pub.Marshal())
	return SSHKey{
		ID: hex.EncodeToString(sum[:8]), Name: name, Type: pub.Type(), Fingerprint: ssh.FingerprintSHA256(pub),
		Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
	}, nil
}

// SSHKeys lists an account's keys.
func (s *Service) SSHKeys(username string) ([]SSHKey, error) {
	a, ok := s.account(username)
	if !ok {
		return nil, ErrNoAccount
	}
	if a.SSHKeys == nil {
		return []SSHKey{}, nil
	}
	return a.SSHKeys, nil
}

// AddSSHKey lets a public key sign in as an account.
func (s *Service) AddSSHKey(username, line, name string) (SSHKey, error) {
	k, err := parseSSHKey(line, name)
	if err != nil {
		return SSHKey{}, err
	}
	k.Added = s.now().UTC()
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return SSHKey{}, ErrNoAccount
	}
	if slices.ContainsFunc(list[i].SSHKeys, func(o SSHKey) bool { return o.ID == k.ID }) {
		return SSHKey{}, errors.New("this account already has that key")
	}
	if len(list[i].SSHKeys) >= maxSSHKeys {
		return SSHKey{}, fmt.Errorf("an account can have up to %d SSH keys", maxSSHKeys)
	}
	list[i].SSHKeys = append(list[i].SSHKeys, k)
	if err := s.secrets.Set(secretAccounts, list); err != nil {
		return SSHKey{}, err
	}
	s.log.Info("added SSH key", "user", list[i].Username, "fingerprint", k.Fingerprint)
	return k, nil
}

// DeleteSSHKey stops a key signing in, and ends what it signed in.
func (s *Service) DeleteSSHKey(username, id string) error {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return ErrNoAccount
	}
	j := slices.IndexFunc(list[i].SSHKeys, func(k SSHKey) bool { return k.ID == id })
	if j < 0 {
		return ErrNoSSHKey
	}
	fp := list[i].SSHKeys[j].Fingerprint
	list[i].SSHKeys = slices.Delete(slices.Clone(list[i].SSHKeys), j, j+1)
	if err := s.secrets.Set(secretAccounts, list); err != nil {
		return err
	}
	s.log.Info("removed SSH key", "user", list[i].Username, "fingerprint", fp)
	return nil
}

// sshChallenge is what a signature answers. It's signed by hlm, so hlm needn't
// keep anything for people who only ask for one.
type sshChallenge struct {
	User    string    `json:"user"`
	Nonce   string    `json:"nonce"`
	Expires time.Time `json:"expires"`
}

// SSHChallenge makes the text to sign to sign in as username. It looks the
// same whether or not there's such an account.
func (s *Service) SSHChallenge(username string) (string, time.Time, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	c := sshChallenge{strings.ToLower(strings.TrimSpace(username)), hex.EncodeToString(nonce), s.now().Add(sshChallengeLife).UTC()}
	text, err := s.sign("ssh-challenge", c)
	return text, c.Expires, err
}

// sshToken is what an SSH sign-in gives. It's checked against the account
// every time, so it lasts only as long as the key and the password do.
type sshToken struct {
	User        string    `json:"user"`
	Fingerprint string    `json:"fingerprint"`
	Version     int64     `json:"version"`
	Expires     time.Time `json:"expires"`
}

// SSHLogin swaps a signature of a challenge for a token. A challenge works
// once.
func (s *Service) SSHLogin(challenge, signature string) (string, time.Time, error) {
	var c sshChallenge
	now := s.now()
	if !s.open("ssh-challenge", challenge, &c) || !now.Before(c.Expires) {
		return "", time.Time{}, ErrBadSSHLogin
	}
	pub, err := verifySSHSignature(signature, []byte(challenge))
	if err != nil {
		s.log.Warn("refused SSH sign-in", "user", c.User, "err", err)
		return "", time.Time{}, ErrBadSSHLogin
	}
	fp := ssh.FingerprintSHA256(pub)
	a, ok := s.account(c.User)
	if !ok || !a.hasKey(fp) {
		s.log.Warn("refused SSH sign-in", "user", c.User, "fingerprint", fp)
		return "", time.Time{}, ErrBadSSHLogin
	}
	if !s.spendChallenge(c, now) {
		return "", time.Time{}, ErrBadSSHLogin
	}
	t := sshToken{a.Username, fp, a.version(), now.Add(sshTokenLife).UTC()}
	v, err := s.sign("ssh-token", t)
	if err != nil {
		return "", time.Time{}, err
	}
	s.noteSSHKeyUse(a.Username, fp, now)
	s.log.Info("signed in", "user", a.Username, "method", "ssh", "fingerprint", fp)
	return SSHTokenPrefix + v, t.Expires, nil
}

func (a Account) hasKey(fingerprint string) bool {
	return slices.ContainsFunc(a.SSHKeys, func(k SSHKey) bool {
		return subtle.ConstantTimeCompare([]byte(k.Fingerprint), []byte(fingerprint)) == 1
	})
}

// spendChallenge records a used challenge until it expires anyway, and
// reports whether it was unused. Only verified signatures get here, so
// strangers can't fill this up.
func (s *Service) spendChallenge(c sshChallenge, now time.Time) bool {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	if s.sshSpent == nil {
		s.sshSpent = map[string]time.Time{}
	}
	for nonce, until := range s.sshSpent {
		if !now.Before(until) {
			delete(s.sshSpent, nonce)
		}
	}
	if _, used := s.sshSpent[c.Nonce]; used {
		return false
	}
	s.sshSpent[c.Nonce] = c.Expires
	return true
}

func (s *Service) noteSSHKeyUse(username, fingerprint string, now time.Time) {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return
	}
	j := slices.IndexFunc(list[i].SSHKeys, func(k SSHKey) bool { return k.Fingerprint == fingerprint })
	if j < 0 {
		return
	}
	list[i].SSHKeys = slices.Clone(list[i].SSHKeys)
	list[i].SSHKeys[j].LastUsed = now.UTC()
	if err := s.secrets.Set(secretAccounts, list); err != nil {
		s.log.Warn("save SSH key's last use", "err", err)
	}
}

// CheckSSHToken returns who a token from an SSH sign-in acts as: its account,
// with the role that has now.
func (s *Service) CheckSSHToken(token string) (rbac.Principal, bool) {
	rest, ok := strings.CutPrefix(token, SSHTokenPrefix)
	if !ok {
		return rbac.Principal{}, false
	}
	var t sshToken
	if !s.open("ssh-token", rest, &t) || !s.now().Before(t.Expires) {
		return rbac.Principal{}, false
	}
	a, ok := s.account(t.User)
	if !ok || a.version() != t.Version || !a.hasKey(t.Fingerprint) {
		return rbac.Principal{}, false
	}
	p := s.Principal(User{Method: MethodLocal, Username: a.Username})
	p.Name = "ssh:" + a.Username
	return p, true
}

// sshSignature is the body of an OpenSSH signature (see PROTOCOL.sshsig).
type sshSignature struct {
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  string
	HashAlgo  string
	Signature []byte
}

const sshSigMagic = "SSHSIG"

// verifySSHSignature checks an armored signature from `ssh-keygen -Y sign`
// and returns the key that made it. Whether that key means anything is up to
// the caller.
func verifySSHSignature(armored string, message []byte) (ssh.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(armored) + "\n"))
	if block == nil || block.Type != "SSH SIGNATURE" {
		return nil, errors.New("not an SSH signature")
	}
	body, ok := strings.CutPrefix(string(block.Bytes), sshSigMagic)
	if !ok {
		return nil, errors.New("not an SSH signature")
	}
	var in sshSignature
	if err := ssh.Unmarshal([]byte(body), &in); err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
	if in.Version != 1 || in.Namespace != SSHNamespace || in.Reserved != "" {
		return nil, fmt.Errorf("signature is for %q, version %d", in.Namespace, in.Version)
	}
	var h hash.Hash
	switch in.HashAlgo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return nil, fmt.Errorf("hash %q isn't supported", in.HashAlgo)
	}
	h.Write(message)
	pub, err := ssh.ParsePublicKey(in.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(in.Signature, &sig); err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
	if sig.Format == ssh.KeyAlgoRSA {
		return nil, errors.New("SHA-1 RSA signatures aren't accepted")
	}
	signed := append([]byte(sshSigMagic), ssh.Marshal(struct {
		Namespace, Reserved, HashAlgo string
		Hash                          []byte
	}{in.Namespace, in.Reserved, in.HashAlgo, h.Sum(nil)})...)
	if err := pub.Verify(signed, &sig); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	return pub, nil
}
