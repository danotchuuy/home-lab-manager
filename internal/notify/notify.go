// Package notify sends alerts about what hlm sees happen (a server crashing,
// a backup failing, an update being available) to notification targets:
// Discord, ntfy, Gotify, a generic webhook or email.
//
// Other packages report events with Notify, which never blocks: events are
// queued and sent by Run. Repeats of the same event are held back for a
// while (see Kind.Repeat), and each target chooses which events it wants,
// for which servers, and when to stay quiet.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Severity is how serious an event is.
type Severity string

const (
	Info    Severity = "info"
	Warning Severity = "warning"
	Error   Severity = "error"
)

// Event is something worth telling people about.
type Event struct {
	Kind     string   `json:"kind"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Message  string   `json:"message,omitempty"`
	// Server is the server the event is about, if any; targets can be
	// limited to some servers.
	Server string `json:"server,omitempty"`
	// Link is a path in the web UI, such as /servers/valheim; it's sent as a
	// full URL when hlm's address is known.
	Link   string            `json:"link,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
	Time   time.Time         `json:"time"`
	// Key identifies repeats of the event; see Kind.Repeat. It defaults to
	// the kind and server.
	Key string `json:"-"`
}

func (e Event) key() string {
	if e.Key != "" {
		return e.Key
	}
	return DedupeKey(e.Kind, e.Server)
}

// DedupeKey is the default Key of an event of kind about server.
func DedupeKey(kind, server string) string { return kind + "/" + server }

// Notifier is what other packages report events to. A nil Notifier is
// allowed wherever one is used, and drops everything.
type Notifier interface {
	// Notify queues an event to be sent. It never blocks.
	Notify(Event)
	// Clear forgets that an event with key was sent, so the next one is sent
	// right away rather than held back as a repeat: e.g. after a backup that
	// failed succeeds again.
	Clear(key string)
}

// Settings are the notification targets and general settings, stored
// encrypted (targets hold webhook URLs, tokens and passwords).
type Settings struct {
	Targets []Target `json:"targets"`
	// BaseURL is hlm's address as people reach it, for links in messages.
	// Empty means the HTTPS address if HTTPS is set up, or no links.
	BaseURL string `json:"baseUrl,omitempty"`
	// DiskPercent is how full a filesystem gets before disk.low is sent;
	// 0 means the default.
	DiskPercent int `json:"diskPercent,omitempty"`
}

// DefaultDiskPercent is the default DiskPercent.
const DefaultDiskPercent = 90

// DiskThreshold returns DiskPercent or its default.
func (s Settings) DiskThreshold() int {
	if s.DiskPercent <= 0 || s.DiskPercent > 100 {
		return DefaultDiskPercent
	}
	return s.DiskPercent
}

const secretName = "notify"

// Delivery is the result of sending one event to one target.
type Delivery struct {
	Target string    `json:"target"` // target ID
	Name   string    `json:"name"`   // target name, as it was
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	Held   bool      `json:"held,omitempty"` // waiting for quiet hours to end
	At     time.Time `json:"at"`
}

// Record is an event and where it went, for the history shown on the
// System page.
type Record struct {
	Event
	// Suppressed is set when the event repeated one sent recently and so
	// wasn't sent again.
	Suppressed bool       `json:"suppressed,omitempty"`
	Deliveries []Delivery `json:"deliveries"`
}

// historySize is how many events the history keeps.
const historySize = 200

// queueSize is how many events can wait to be sent; more are dropped.
const queueSize = 256

// Options configures a Service.
type Options struct {
	Secrets *secrets.Store
	// StatePath is a JSON file for what has been sent (so repeats are held
	// back across restarts) and the history.
	StatePath string
	Log       *slog.Logger
	// HTTPClient sends webhook requests; the default has a 15 second
	// timeout.
	HTTPClient *http.Client
	// BaseURL returns hlm's address when Settings.BaseURL is empty, e.g.
	// https://hlm.example.com when HTTPS is set up.
	BaseURL func() string
	// GroupServers returns the servers in a server group, for targets
	// limited to groups.
	GroupServers func(groupID string) []string
	// Now and RetryWait are for tests.
	Now       func() time.Time
	RetryWait func(attempt int) time.Duration
}

// Service keeps the targets and sends events to them.
type Service struct {
	opts    Options
	senders map[string]sender
	queue   chan Event

	saveMu sync.Mutex // serializes changes to the stored settings

	mu      sync.Mutex
	state   state
	dropped int
	// held are events kept back during a target's quiet hours, by target
	// ID.
	held map[string][]Event
	// last is the latest delivery result per target ID.
	last map[string]Delivery
}

// New creates the service and loads its state.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.RetryWait == nil {
		o.RetryWait = func(attempt int) time.Duration { return time.Duration(attempt*attempt) * 5 * time.Second }
	}
	s := &Service{
		opts:  o,
		queue: make(chan Event, queueSize),
		held:  map[string][]Event{},
		last:  map[string]Delivery{},
	}
	s.senders = map[string]sender{
		KindDiscord: discordSender{s.opts.HTTPClient},
		KindNtfy:    ntfySender{s.opts.HTTPClient},
		KindGotify:  gotifySender{s.opts.HTTPClient},
		KindWebhook: webhookSender{s.opts.HTTPClient},
		KindEmail:   emailSender{},
	}
	s.state = loadState(o.StatePath, o.Log)
	for _, r := range s.state.History {
		for _, d := range r.Deliveries {
			if !d.Held {
				s.last[d.Target] = d
			}
		}
	}
	return s
}

// Settings returns the stored settings.
func (s *Service) Settings() (Settings, error) {
	var set Settings
	_, err := s.opts.Secrets.Get(secretName, &set)
	return set, err
}

func (s *Service) saveSettings(set Settings) error {
	return s.opts.Secrets.Set(secretName, set)
}

// Notify queues an event. It never blocks: when the queue is full, the
// event is dropped.
func (s *Service) Notify(e Event) {
	if s == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = s.opts.Now()
	}
	if e.Severity == "" {
		if k, ok := KindByName(e.Kind); ok {
			e.Severity = k.Severity
		} else {
			e.Severity = Info
		}
	}
	e.Message = Redact(e.Message)
	e.Title = Redact(e.Title)
	select {
	case s.queue <- e:
	default:
		s.mu.Lock()
		s.dropped++
		n := s.dropped
		s.mu.Unlock()
		if n == 1 || n%100 == 0 {
			s.opts.Log.Warn("notification queue is full; dropping events", "dropped", n)
		}
	}
}

// Clear forgets that the event with key was sent; see Notifier.
func (s *Service) Clear(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	_, ok := s.state.Sent[key]
	delete(s.state.Sent, key)
	s.mu.Unlock()
	if ok {
		s.saveState()
	}
}

// Run sends queued events until ctx ends.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.queue:
			s.handle(ctx, e)
		case <-tick.C:
			s.releaseHeld(ctx)
		}
	}
}

// handle decides whether an event is a repeat, and sends it to each target
// that wants it.
func (s *Service) handle(ctx context.Context, e Event) {
	now := s.opts.Now()
	rec := Record{Event: e, Deliveries: []Delivery{}}
	kind, _ := KindByName(e.Kind)
	s.mu.Lock()
	if kind.Repeat != 0 {
		if at, ok := s.state.Sent[e.key()]; ok && (kind.Repeat < 0 || now.Sub(at) < kind.Repeat) {
			rec.Suppressed = true
		} else {
			s.state.Sent[e.key()] = now
		}
	}
	s.mu.Unlock()
	if rec.Suppressed {
		s.opts.Log.Debug("notification held back as a repeat", "kind", e.Kind, "server", e.Server)
		s.record(rec)
		return
	}

	set, err := s.Settings()
	if err != nil {
		s.opts.Log.Warn("read notification settings", "err", err)
		return
	}
	for _, t := range set.Targets {
		if !s.wants(t, e) {
			continue
		}
		if t.quiet(now) && e.Severity != Error {
			s.mu.Lock()
			if len(s.held[t.ID]) < queueSize {
				s.held[t.ID] = append(s.held[t.ID], e)
			}
			s.mu.Unlock()
			rec.Deliveries = append(rec.Deliveries, Delivery{Target: t.ID, Name: t.Name, Held: true, At: now})
			continue
		}
		rec.Deliveries = append(rec.Deliveries, s.deliver(ctx, set, t, e))
	}
	s.record(rec)
}

// wants reports whether target t should get event e.
func (s *Service) wants(t Target, e Event) bool {
	if !t.Enabled || !slices.Contains(t.Events, e.Kind) {
		return false
	}
	if e.Server == "" || (len(t.Servers) == 0 && len(t.Groups) == 0) {
		return true
	}
	if slices.Contains(t.Servers, e.Server) {
		return true
	}
	if s.opts.GroupServers != nil {
		for _, g := range t.Groups {
			if slices.Contains(s.opts.GroupServers(g), e.Server) {
				return true
			}
		}
	}
	return false
}

// deliver sends e to t, retrying a few times, and records the result.
func (s *Service) deliver(ctx context.Context, set Settings, t Target, e Event) Delivery {
	snd, ok := s.senders[t.Kind]
	d := Delivery{Target: t.ID, Name: t.Name}
	if !ok {
		d.Error = "unknown kind of target: " + t.Kind
	} else {
		msg := s.message(set, e)
		var err error
		for attempt := 1; ; attempt++ {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = snd.send(cctx, t, msg)
			cancel()
			var perm *permanentError
			if err == nil || errors.As(err, &perm) || attempt == 3 || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(s.opts.RetryWait(attempt)):
			}
		}
		if err != nil {
			d.Error = Redact(err.Error())
			s.opts.Log.Warn("send notification", "target", t.Name, "kind", e.Kind, "err", d.Error)
		} else {
			d.OK = true
		}
	}
	d.At = s.opts.Now()
	s.mu.Lock()
	s.last[t.ID] = d
	s.mu.Unlock()
	return d
}

// message is an event as sent: with a full link, if hlm's address is known.
type message struct {
	Event
	URL string // Link as a full URL, or empty
}

func (s *Service) message(set Settings, e Event) message {
	m := message{Event: e}
	base := strings.TrimRight(set.BaseURL, "/")
	if base == "" && s.opts.BaseURL != nil {
		base = strings.TrimRight(s.opts.BaseURL(), "/")
	}
	if base != "" && e.Link != "" {
		m.URL = base + e.Link
	}
	return m
}

// releaseHeld sends events held for quiet hours once they're over.
func (s *Service) releaseHeld(ctx context.Context) {
	s.mu.Lock()
	if len(s.held) == 0 {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	set, err := s.Settings()
	if err != nil {
		return
	}
	now := s.opts.Now()
	for _, t := range set.Targets {
		if t.quiet(now) {
			continue
		}
		s.mu.Lock()
		events := s.held[t.ID]
		delete(s.held, t.ID)
		s.mu.Unlock()
		for _, e := range events {
			s.deliver(ctx, set, t, e)
		}
	}
	// Forget events for targets that were removed.
	s.mu.Lock()
	for id := range s.held {
		if !slices.ContainsFunc(set.Targets, func(t Target) bool { return t.ID == id }) {
			delete(s.held, id)
		}
	}
	s.mu.Unlock()
}

func (s *Service) record(r Record) {
	s.mu.Lock()
	s.state.History = append(s.state.History, r)
	if over := len(s.state.History) - historySize; over > 0 {
		s.state.History = slices.Delete(s.state.History, 0, over)
	}
	s.mu.Unlock()
	s.saveState()
}

// History returns the latest events, newest first.
func (s *Service) History() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.state.History))
	for i, r := range s.state.History {
		out[len(out)-1-i] = r
	}
	return out
}

// Test sends a test message to a target, given as it would be saved (blank
// secrets are taken from the saved target with the same ID).
func (s *Service) Test(ctx context.Context, in Target) error {
	set, err := s.Settings()
	if err != nil {
		return err
	}
	var old *Target
	if i := set.index(in.ID); i >= 0 {
		old = &set.Targets[i]
	}
	t, err := in.merge(old)
	if err != nil {
		return err
	}
	snd, ok := s.senders[t.Kind]
	if !ok {
		return fmt.Errorf("unknown kind of target: %s", t.Kind)
	}
	e := Event{
		Kind: KindTest, Severity: Info, Time: s.opts.Now(), Link: "/system#notifications",
		Title:   "Test notification from Home Lab Manager",
		Message: "If you can read this, notifications to " + t.Name + " work.",
	}
	err = snd.send(ctx, t, s.message(set, e))
	if err != nil {
		return errors.New(Redact(err.Error()))
	}
	return nil
}
