package auth

// A download link can't carry an Authorization header, so someone signed in
// with a token gets a ticket: a URL parameter that works once, for one URL,
// from one address, for a minute.

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

const ticketLife = time.Minute

type ticket struct {
	who     rbac.Principal
	target  string // path and query, without the ticket
	remote  string // address, without the port
	expires time.Time
}

type tickets struct {
	mu   sync.Mutex
	byID map[string]ticket
}

// IssueTicket returns a ticket for p to GET target (a path and query) once,
// from remote.
func (s *Service) IssueTicket(p rbac.Principal, target, remote string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	a, _ := remoteAddr(remote)
	now := s.now()
	s.tickets.mu.Lock()
	defer s.tickets.mu.Unlock()
	if s.tickets.byID == nil {
		s.tickets.byID = map[string]ticket{}
	}
	for k, t := range s.tickets.byID {
		if !now.Before(t.expires) {
			delete(s.tickets.byID, k)
		}
	}
	s.tickets.byID[id] = ticket{p, target, a.String(), now.Add(ticketLife)}
	return id, nil
}

// RedeemTicket uses up a ticket and returns who it's for, if it's for target
// and remote and hasn't expired.
func (s *Service) RedeemTicket(id, target, remote string) (rbac.Principal, bool) {
	a, _ := remoteAddr(remote)
	s.tickets.mu.Lock()
	defer s.tickets.mu.Unlock()
	t, ok := s.tickets.byID[id]
	delete(s.tickets.byID, id)
	if !ok || t.target != target || t.remote != a.String() || !s.now().Before(t.expires) {
		return rbac.Principal{}, false
	}
	return t.who, true
}
