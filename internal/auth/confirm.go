package auth

// Changes that could give someone lasting access, like a new API key, take a
// person at a browser: a session cookie whose password was confirmed in the
// last ConfirmWindow, or, for single sign-on, a fresh sign-in with the
// provider. A stolen API key or SSH token can't make itself permanent.

import (
	"errors"
	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"net/http"
	"time"
)

// ConfirmWindow is how long a confirmation lasts.
const ConfirmWindow = 10 * time.Minute

// Errors from confirming.
var (
	ErrNoSession      = errors.New("this needs you signed in in a browser, not an API key or token")
	ErrConfirmWithSSO = errors.New("you signed in with single sign-on, so confirm by signing in there again")
)

// Confirmed reports whether a session's sign-in was confirmed recently
// enough for changes to access.
func (s *Service) Confirmed(u User) bool {
	now := s.now()
	return !u.Confirmed.IsZero() && now.Sub(u.Confirmed) < ConfirmWindow && !u.Confirmed.After(now.Add(time.Minute))
}

// ConfirmPassword confirms the request's session with its account's password.
func (s *Service) ConfirmPassword(w http.ResponseWriter, r *http.Request, password string) (User, error) {
	u, ok := s.Session(r)
	if !ok {
		return User{}, ErrNoSession
	}
	if u.Method != MethodLocal {
		return User{}, ErrConfirmWithSSO
	}
	if err := s.VerifyPassword(u.Username, password); err != nil {
		s.log.Warn("refused confirmation", "user", u.Username, "remote", r.RemoteAddr)
		s.record(audit.Entry{Actor: u.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.confirm",
			Result: audit.ResultDenied, Error: "wrong password"})
		return User{}, err
	}
	u.Confirmed = s.now().UTC()
	if err := s.startSession(w, u, secureRequest(r)); err != nil {
		return User{}, err
	}
	s.log.Info("confirmed sign-in", "user", u.Username, "method", MethodLocal, "remote", r.RemoteAddr)
	s.record(audit.Entry{Actor: u.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.confirm"})
	return u, nil
}
