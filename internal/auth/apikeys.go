package auth

// API keys, for scripts: sent as "Authorization: Bearer <key>", like
// HLM_TOKEN. A key is hlm_<id>_<secret>; only the SHA-256 of the secret is
// kept, so a key is shown once, when it's made. A key has a global role, and
// a member key only what server groups grant it.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

const (
	secretAPIKeys = "auth-api-keys"

	// KeyPrefix starts every API key, so they're easy to recognize (and
	// to find if one leaks).
	KeyPrefix = "hlm_"

	maxKeyDays = 3650
	// keyUseSaveEvery limits how often a key's last use is written to disk.
	keyUseSaveEvery = time.Hour
)

// APIKey is a stored API key. Only the hash of its secret is kept.
type APIKey struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"` // hex SHA-256 of the secret
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires,omitzero"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
	// Role is the key's global role. Keys made before roles existed have
	// none, and keep their full access; see role.
	Role rbac.Role `json:"role,omitempty"`
	// AllowFrom limits where the key can be used from; empty is anywhere.
	AllowFrom []string `json:"allowFrom,omitempty"`
}

// role returns the key's global role. A key without one predates roles,
// when every key could do everything.
func (k APIKey) role() rbac.Role {
	if k.Role == rbac.None {
		return rbac.Admin
	}
	return k.Role
}

// APIKeyInfo is an API key without its hash, for the UI.
type APIKeyInfo struct {
	ID string `json:"id"`
	// Prefix is the start of the key, to tell keys apart.
	Prefix    string    `json:"prefix"`
	Name      string    `json:"name"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires,omitzero"`
	Expired   bool      `json:"expired,omitempty"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
	Role      rbac.Role `json:"role"`
	AllowFrom []string  `json:"allowFrom"`
}

func (s *Service) apiKeys() []APIKey {
	var list []APIKey
	if _, err := s.secrets.Get(secretAPIKeys, &list); err != nil {
		s.log.Error("read API keys", "err", err)
	}
	return list
}

// APIKeys lists the API keys, newest first.
func (s *Service) APIKeys() []APIKeyInfo {
	out := []APIKeyInfo{}
	now := s.now()
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	for _, k := range s.apiKeys() {
		info := APIKeyInfo{
			ID: k.ID, Prefix: KeyPrefix + k.ID + "_…", Name: k.Name, Created: k.Created, Expires: k.Expires,
			Expired: !k.Expires.IsZero() && !now.Before(k.Expires), LastUsed: k.LastUsed, Role: k.role(),
			AllowFrom: orEmpty(k.AllowFrom),
		}
		if seen := s.keySeen[k.ID]; seen.After(info.LastUsed) {
			info.LastUsed = seen
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b APIKeyInfo) int { return b.Created.Compare(a.Created) })
	return out
}

// CreateAPIKey makes a key named name with a global role (a member if none
// is given) that expires after days (0: never). The returned key is the only
// time its secret is available.
func (s *Service) CreateAPIKey(name string, days int, role rbac.Role, allowFrom []string) (string, APIKeyInfo, error) {
	from, err := parseAllowFrom(allowFrom)
	if err != nil {
		return "", APIKeyInfo{}, err
	}
	if role == rbac.None {
		role = rbac.Member
	}
	if !role.ForKey() {
		return "", APIKeyInfo{}, errors.New("a key's role is admin, deployer, member or metrics")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", APIKeyInfo{}, errors.New("give the key a name of up to 100 characters, such as what will use it")
	}
	if days < 0 || days > maxKeyDays {
		return "", APIKeyInfo{}, errors.New("a key can expire after 1 to 3650 days, or never")
	}
	id := make([]byte, 6)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", APIKeyInfo{}, err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", APIKeyInfo{}, err
	}
	sec := base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(sec))
	now := s.now().UTC()
	k := APIKey{ID: hex.EncodeToString(id), Name: name, Hash: hex.EncodeToString(sum[:]), Created: now, Role: role, AllowFrom: from}
	if days > 0 {
		k.Expires = now.Add(time.Duration(days) * 24 * time.Hour)
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if err := s.secrets.Set(secretAPIKeys, append(s.apiKeys(), k)); err != nil {
		return "", APIKeyInfo{}, err
	}
	s.log.Info("created API key", "id", k.ID, "name", k.Name)
	return KeyPrefix + k.ID + "_" + sec, APIKeyInfo{ID: k.ID, Prefix: KeyPrefix + k.ID + "_…", Name: k.Name, Created: k.Created, Expires: k.Expires, Role: role, AllowFrom: orEmpty(from)}, nil
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// SetAPIKeyAllowFrom changes where a key can be used from.
func (s *Service) SetAPIKeyAllowFrom(id string, allowFrom []string) (APIKeyInfo, error) {
	from, err := parseAllowFrom(allowFrom)
	if err != nil {
		return APIKeyInfo{}, err
	}
	s.keyMu.Lock()
	list := s.apiKeys()
	i := slices.IndexFunc(list, func(k APIKey) bool { return k.ID == id })
	if i < 0 {
		s.keyMu.Unlock()
		return APIKeyInfo{}, ErrNoAPIKey
	}
	list[i].AllowFrom = from
	err = s.secrets.Set(secretAPIKeys, list)
	s.keyMu.Unlock()
	if err != nil {
		return APIKeyInfo{}, err
	}
	s.log.Info("changed where an API key can be used from", "id", id, "allowFrom", from)
	for _, k := range s.APIKeys() {
		if k.ID == id {
			return k, nil
		}
	}
	return APIKeyInfo{}, ErrNoAPIKey
}

// ErrNoAPIKey means there's no API key with that ID.
var ErrNoAPIKey = errors.New("there's no API key with that ID")

// DeleteAPIKey revokes a key.
func (s *Service) DeleteAPIKey(id string) error {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	list := s.apiKeys()
	i := slices.IndexFunc(list, func(k APIKey) bool { return k.ID == id })
	if i < 0 {
		return ErrNoAPIKey
	}
	name := list[i].Name
	if err := s.secrets.Set(secretAPIKeys, slices.Delete(list, i, i+1)); err != nil {
		return err
	}
	delete(s.keySeen, id)
	delete(s.keySaved, id)
	s.log.Info("revoked API key", "id", id, "name", name)
	return nil
}

// CheckAPIKey returns who a valid, unexpired API key acts as, and notes its
// use.
func (s *Service) CheckAPIKey(token, remote string) (rbac.Principal, bool) {
	rest, ok := strings.CutPrefix(token, KeyPrefix)
	if !ok {
		return rbac.Principal{}, false
	}
	id, sec, ok := strings.Cut(rest, "_")
	if !ok || id == "" || sec == "" {
		return rbac.Principal{}, false
	}
	sum := sha256.Sum256([]byte(sec))
	got := hex.EncodeToString(sum[:])
	now := s.now()
	for _, k := range s.apiKeys() {
		if k.ID != id {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(k.Hash)) != 1 || (!k.Expires.IsZero() && !now.Before(k.Expires)) {
			return rbac.Principal{}, false
		}
		if !allowedFrom(k.AllowFrom, remote) {
			s.log.Warn("refused API key from an address it isn't allowed from", "key", k.Name, "remote", remote)
			return rbac.Principal{}, false
		}
		s.noteKeyUse(id, now)
		return rbac.Principal{
			Name: "key:" + k.Name, Role: k.role(),
			Subjects: []rbac.Subject{{Kind: rbac.SubjectKey, Name: k.ID}},
		}, true
	}
	return rbac.Principal{}, false
}

// noteKeyUse records a key's use, saving it at most every keyUseSaveEvery.
func (s *Service) noteKeyUse(id string, now time.Time) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	s.keySeen[id] = now
	if now.Sub(s.keySaved[id]) < keyUseSaveEvery {
		return
	}
	s.keySaved[id] = now
	list := s.apiKeys()
	if i := slices.IndexFunc(list, func(k APIKey) bool { return k.ID == id }); i >= 0 {
		list[i].LastUsed = now.UTC()
		if err := s.secrets.Set(secretAPIKeys, list); err != nil {
			s.log.Warn("save API key's last use", "err", err)
		}
	}
}
