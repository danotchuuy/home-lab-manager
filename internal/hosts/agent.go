package hosts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
)

// Hosts with an agent: "hlm agent" on the host connects out to hlm, which
// reaches the host's Docker through it (see package agent). They're added
// by enrolment: an admin makes a single-use token for the host here, and
// the agent trades it for its own credential the first time it runs.

const (
	// secretEnrolments are the tokens not used yet.
	secretEnrolments = "host-agent-enrolments"
	// secretAgentPrefix keeps the hash of a host's agent credential.
	secretAgentPrefix = "host-agent-secret-"
	// EnrolmentTTL is how long an enrolment token works.
	EnrolmentTTL = time.Hour
	tokenPrefix  = "hlme_"
)

// ErrBadToken is returned for an enrolment token that isn't valid (any
// more).
var ErrBadToken = errors.New("this enrolment token isn't valid: it was used already, has expired (they last an hour), or was replaced by a newer one; make a new one on hlm's Hosts page")

type enrolment struct {
	Host    Host      `json:"host"`
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// Enrolment is a token made for a host's agent.
type Enrolment struct {
	Token   string    `json:"token"`
	Host    Host      `json:"host"`
	Expires time.Time `json:"expires"`
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (r *Registry) enrolments(now time.Time) ([]enrolment, error) {
	var list []enrolment
	if _, err := r.opts.Secrets.Get(secretEnrolments, &list); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(e enrolment) bool { return !now.Before(e.Expires) }), nil
}

// CreateEnrolment makes a single-use token for a new host with an agent.
// It replaces an earlier token for the same host ID.
func (r *Registry) CreateEnrolment(in Host) (Enrolment, error) {
	if r.opts.Agents == nil {
		return Enrolment{}, errors.New("agents can't connect to this hlm")
	}
	h := in
	h.Kind = KindAgent
	if err := validate(&h, ""); err != nil {
		return Enrolment{}, err
	}
	r.saveM.Lock()
	defer r.saveM.Unlock()
	if _, err := r.Get(h.ID); err == nil {
		return Enrolment{}, fmt.Errorf("there's already a host %q", h.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return Enrolment{}, err
	}
	now := time.Now().UTC()
	list, err := r.enrolments(now)
	if err != nil {
		return Enrolment{}, err
	}
	list = slices.DeleteFunc(list, func(e enrolment) bool { return e.Host.ID == h.ID })
	token := tokenPrefix + randomHex(32)
	e := enrolment{Host: h, Hash: hashSecret(token), Expires: now.Add(EnrolmentTTL)}
	if err := r.opts.Secrets.Set(secretEnrolments, append(list, e)); err != nil {
		return Enrolment{}, err
	}
	r.opts.Log.Info("made an agent enrolment token", "host", h.ID, "expires", e.Expires)
	return Enrolment{Token: token, Host: h, Expires: e.Expires}, nil
}

// Enrol trades an enrolment token for the host it was made for, and the
// agent's credential (only its hash is kept).
func (r *Registry) Enrol(token string) (Host, string, error) {
	r.saveM.Lock()
	defer r.saveM.Unlock()
	now := time.Now().UTC()
	list, err := r.enrolments(now)
	if err != nil {
		return Host{}, "", err
	}
	hash := hashSecret(token)
	i := slices.IndexFunc(list, func(e enrolment) bool { return subtle.ConstantTimeCompare([]byte(e.Hash), []byte(hash)) == 1 })
	if i < 0 {
		return Host{}, "", ErrBadToken
	}
	h := list[i].Host
	// Single use, whatever happens next.
	if err := r.opts.Secrets.Set(secretEnrolments, slices.Delete(list, i, i+1)); err != nil {
		return Host{}, "", err
	}
	if _, err := r.Get(h.ID); err == nil {
		return Host{}, "", fmt.Errorf("there's already a host %q", h.ID)
	}
	secret := randomHex(32)
	h.Created, h.Updated = now, now
	hosts, err := r.load()
	if err != nil {
		return Host{}, "", err
	}
	if err := r.opts.Secrets.Set(secretAgentPrefix+h.ID, hashSecret(secret)); err != nil {
		return Host{}, "", err
	}
	if err := r.opts.Secrets.Set(secretHosts, append(hosts, h)); err != nil {
		return Host{}, "", err
	}
	c, err := r.connectWith(h, "")
	if err != nil {
		return Host{}, "", err
	}
	r.mu.Lock()
	r.conns[h.ID] = c
	r.mu.Unlock()
	r.opts.Log.Info("enrolled a host's agent", "host", h.ID)
	r.changed()
	return h, agent.Credential(h.ID, secret), nil
}

// authenticateAgent checks an agent's credential.
func (r *Registry) authenticateAgent(id, secret string) bool {
	h, err := r.Get(id)
	if err != nil || h.Kind != KindAgent {
		return false
	}
	var want string
	if ok, err := r.opts.Secrets.Get(secretAgentPrefix+id, &want); err != nil || !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(want)) == 1
}

// agentChanged hears an agent connect or go: connections kept from the
// last time it was connected went with it.
func (r *Registry) agentChanged(id string, _ bool) {
	r.mu.Lock()
	c := r.conns[id]
	r.mu.Unlock()
	if c != nil {
		c.client.Close()
	}
}

// AgentStatus is a host's agent's connection.
func (r *Registry) AgentStatus(id string) *agent.Status {
	if r.opts.Agents == nil {
		return nil
	}
	st := r.opts.Agents.Status(id)
	return &st
}

func (r *Registry) agentDial(id string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		if r.opts.Agents == nil {
			return nil, agent.ErrNotConnected
		}
		return r.opts.Agents.Dial(ctx, id)
	}
}

// ErrNoHostMetrics is returned for a host that doesn't report its own
// CPU, memory, network and disks: only hosts with hlm's agent do.
var ErrNoHostMetrics = errors.New("only hosts with hlm's agent report their CPU, memory, network and disks; hlm sees other hosts through Docker alone")

// HostMetrics asks a host's agent for the host's metrics.
func (r *Registry) HostMetrics(ctx context.Context, id string) (hostmetrics.Snapshot, error) {
	h, err := r.Get(id)
	if err != nil {
		return hostmetrics.Snapshot{}, err
	}
	if h.Kind != KindAgent || r.opts.Agents == nil {
		return hostmetrics.Snapshot{}, ErrNoHostMetrics
	}
	return r.opts.Agents.HostMetrics(ctx, id)
}

// RunHostMetrics samples the hosts with an agent every interval until ctx
// ends, passing each sample (or an unavailable one, saying why) to
// observe.
func (r *Registry) RunHostMetrics(ctx context.Context, every time.Duration, observe func(id string, snap hostmetrics.Snapshot)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := r.List()
		if err != nil {
			continue
		}
		for _, h := range list {
			if h.Kind != KindAgent {
				continue
			}
			snap, err := r.HostMetrics(ctx, h.ID)
			if err != nil {
				snap = hostmetrics.Snapshot{Reason: err.Error()}
			}
			observe(h.ID, snap)
		}
	}
}
