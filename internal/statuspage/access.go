package statuspage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Two optional passwords: one to see the page at all, one for the details
// (players' names, and the address and note for connecting). A visitor who
// gives one gets a signed cookie for that level; changing a password makes
// the cookies for it stop working.

// Levels of access.
const (
	LevelNone    = 0 // the page needs its view password
	LevelView    = 1 // states, players online, versions and uptime
	LevelDetails = 2 // also players' names, addresses and notes
)

// CookieName holds a visitor's access.
const CookieName = "hlm_status"

// UnlockFor is how long an unlocked page stays unlocked.
const UnlockFor = 30 * 24 * time.Hour

const (
	secretKeyName = "statuspage-key"
	minPassword   = 4
	maxPassword   = 128
)

// ErrWrongPassword is returned for a password that's neither.
var ErrWrongPassword = errors.New("wrong password")

// PasswordChange is how a save changes a password: nil keeps it, "" removes
// it, anything else sets it.
type PasswordChange = *string

// checkNewPassword checks a password being set.
func checkNewPassword(p string) error {
	if len(p) < minPassword || len(p) > maxPassword {
		return fmt.Errorf("a password is %d to %d characters", minPassword, maxPassword)
	}
	return nil
}

// key signs access cookies; made once, kept in the secrets store.
func (s *Service) key() ([]byte, error) {
	var k []byte
	if ok, err := s.opts.Secrets.Get(secretKeyName, &k); err != nil {
		return nil, err
	} else if ok && len(k) == 32 {
		return k, nil
	}
	k = make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, s.opts.Secrets.Set(secretKeyName, k)
}

// sign makes a cookie value for a level until expiry, on a site ("" for
// any other address). It's bound to the site and the level's password
// hash, so it's no use on another site, and a new password ends it.
func (s *Service) sign(level int, expiry time.Time, site, hash string) (string, error) {
	k, err := s.key()
	if err != nil {
		return "", err
	}
	payload := strconv.Itoa(level) + "." + strconv.FormatInt(expiry.Unix(), 10)
	m := hmac.New(sha256.New, k)
	m.Write([]byte(payload + "|" + site + "|" + hash))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

// passwords are the hashes that apply on a host, and the site ID they're
// bound to.
func passwords(set Settings, host string) (scope, view, details string) {
	if site := siteFor(set, host); site != nil {
		return site.ID, site.ViewHash, site.DetailsHash
	}
	return "", set.ViewHash, set.DetailsHash
}

// levelOf reads a cookie value: the level it gives on a host, or LevelNone.
func (s *Service) levelOf(set Settings, host, cookie string, now time.Time) int {
	scope, viewHash, detailsHash := passwords(set, host)
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 {
		return LevelNone
	}
	level, err1 := strconv.Atoi(parts[0])
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() > exp {
		return LevelNone
	}
	hash := viewHash
	if level == LevelDetails {
		hash = detailsHash
	}
	if hash == "" || (level != LevelView && level != LevelDetails) {
		return LevelNone
	}
	want, err := s.sign(level, time.Unix(exp, 0), scope, hash)
	if err != nil || !hmac.Equal([]byte(want), []byte(cookie)) {
		return LevelNone
	}
	return level
}

// Access is what a visitor may see, and what's left to unlock.
type Access struct {
	Level int `json:"level"`
	// ViewLocked: the page needs its view password. DetailsLocked: names,
	// addresses and notes need the details password.
	ViewLocked    bool `json:"viewLocked"`
	DetailsLocked bool `json:"detailsLocked"`
	// Unlocked says a password was given (so it can be forgotten).
	Unlocked bool `json:"unlocked"`
}

// AccessFor works out a visitor's access on a host from their cookie: the
// passwords are the host's site's, or the page's on any other address.
func (s *Service) AccessFor(host, cookie string, now time.Time) (Access, error) {
	set, err := s.Settings()
	if err != nil {
		return Access{}, err
	}
	return s.access(set, host, cookie, now), nil
}

func (s *Service) access(set Settings, host, cookie string, now time.Time) Access {
	_, viewHash, detailsHash := passwords(set, host)
	got := s.levelOf(set, host, cookie, now)
	level := got
	if viewHash == "" && level < LevelView {
		level = LevelView
	}
	// Without a details password, whoever sees the page sees the details.
	if detailsHash == "" && level >= LevelView {
		level = LevelDetails
	}
	return Access{Level: level, ViewLocked: level < LevelView, DetailsLocked: level < LevelDetails, Unlocked: got > LevelNone}
}

// Unlock checks a password against a host's: the details one (which also
// lets the page be seen) or the view one. It returns the level and a
// cookie value for it.
func (s *Service) Unlock(host, password string, now time.Time) (int, string, time.Time, error) {
	set, err := s.Settings()
	if err != nil {
		return 0, "", time.Time{}, err
	}
	if !set.Enabled {
		return 0, "", time.Time{}, ErrDisabled
	}
	scope, viewHash, detailsHash := passwords(set, host)
	level, hash := LevelNone, ""
	// Check both, so the time taken doesn't say which matched.
	details := detailsHash != "" && s.matches(detailsHash, password)
	view := viewHash != "" && s.matches(viewHash, password)
	switch {
	case details:
		level, hash = LevelDetails, detailsHash
	case view:
		level, hash = LevelView, viewHash
	default:
		return 0, "", time.Time{}, ErrWrongPassword
	}
	expiry := now.Add(UnlockFor)
	token, err := s.sign(level, expiry, scope, hash)
	return level, token, expiry, err
}

func (s *Service) matches(hash, password string) bool {
	if s.opts.PasswordMatches == nil {
		return false
	}
	return s.opts.PasswordMatches(hash, password)
}

// Redact leaves out what a visitor's access doesn't cover.
func Redact(p Page, a Access) Page {
	if !a.DetailsLocked {
		return p
	}
	out := p
	out.Servers = make([]Server, len(p.Servers))
	for i, srv := range p.Servers {
		srv.PlayerNames, srv.Address, srv.Hint = nil, "", ""
		out.Servers[i] = srv
	}
	return out
}
