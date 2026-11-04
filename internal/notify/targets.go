package notify

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Kinds of target.
const (
	KindDiscord = "discord"
	KindNtfy    = "ntfy"
	KindGotify  = "gotify"
	KindWebhook = "webhook"
	KindEmail   = "email"
)

var targetKinds = []string{KindDiscord, KindNtfy, KindGotify, KindWebhook, KindEmail}

// Target is somewhere notifications are sent.
type Target struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	// Events are the event kinds sent to this target.
	Events []string `json:"events"`
	// Servers and Groups (server group IDs) limit server events to those
	// servers; when both are empty, events about every server are sent.
	// Events not about a server are always sent.
	Servers []string `json:"servers,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	// QuietStart and QuietEnd ("22:00", "07:30", in hlm's time zone) hold
	// back events other than errors until the quiet hours end.
	QuietStart string `json:"quietStart,omitempty"`
	QuietEnd   string `json:"quietEnd,omitempty"`

	// URL is the Discord or generic webhook's URL (secret, since it
	// carries a token), or the ntfy or Gotify server's address.
	URL string `json:"url,omitempty"`
	// Topic is the ntfy topic.
	Topic string `json:"topic,omitempty"`
	// Token is the ntfy access token or Gotify application token (secret).
	Token string `json:"token,omitempty"`
	// Headers are extra headers for the generic webhook; their values are
	// secret.
	Headers []Header `json:"headers,omitempty"`

	// Email over SMTP.
	SMTPHost string `json:"smtpHost,omitempty"`
	SMTPPort int    `json:"smtpPort,omitempty"`
	// Security is starttls (the default), tls (implicit TLS, usually port
	// 465) or none.
	Security string   `json:"security,omitempty"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"` // secret
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
}

// Header is an HTTP header sent to a generic webhook.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// urlIsSecret reports whether a target's URL carries its credentials.
func (t Target) urlIsSecret() bool { return t.Kind == KindDiscord || t.Kind == KindWebhook }

// PublicTarget is a target as the API shows it: without its secrets.
type PublicTarget struct {
	Target
	// URLHint shows a secret URL's scheme and host.
	URLHint     string    `json:"urlHint,omitempty"`
	HasURL      bool      `json:"hasUrl"`
	HasToken    bool      `json:"hasToken"`
	HasPassword bool      `json:"hasPassword"`
	Last        *Delivery `json:"last,omitempty"`
}

func (t Target) public() PublicTarget {
	p := PublicTarget{Target: t, HasURL: t.URL != "", HasToken: t.Token != "", HasPassword: t.Password != ""}
	if t.urlIsSecret() && t.URL != "" {
		p.URLHint = urlHint(t.URL)
		p.Target.URL = ""
	}
	p.Target.Token, p.Target.Password = "", ""
	p.Target.Headers = nil
	for _, h := range t.Headers {
		p.Target.Headers = append(p.Target.Headers, Header{Name: h.Name})
	}
	p.Target.Events = slices.Clone(t.Events)
	return p
}

func urlHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(set)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// PublicSettings are the settings as the API shows them.
type PublicSettings struct {
	Targets     []PublicTarget `json:"targets"`
	BaseURL     string         `json:"baseUrl"`
	DiskPercent int            `json:"diskPercent"`
	// DefaultBaseURL is the address used for links when BaseURL is empty.
	DefaultBaseURL string `json:"defaultBaseUrl,omitempty"`
	Kinds          []Kind `json:"kinds"`
	Dropped        int    `json:"dropped,omitempty"`
}

// Public returns the settings without secrets, with each target's last
// delivery.
func (s *Service) Public() (PublicSettings, error) {
	set, err := s.Settings()
	if err != nil {
		return PublicSettings{}, err
	}
	out := PublicSettings{Targets: []PublicTarget{}, BaseURL: set.BaseURL, DiskPercent: set.DiskThreshold(), Kinds: Kinds}
	if s.opts.BaseURL != nil {
		out.DefaultBaseURL = s.opts.BaseURL()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Dropped = s.dropped
	for _, t := range set.Targets {
		p := t.public()
		if d, ok := s.last[t.ID]; ok {
			p.Last = &d
		}
		out.Targets = append(out.Targets, p)
	}
	return out, nil
}

func (set Settings) index(id string) int {
	if id == "" {
		return -1
	}
	return slices.IndexFunc(set.Targets, func(t Target) bool { return t.ID == id })
}

// ErrNotFound is returned for a target that doesn't exist.
var ErrNotFound = errors.New("no such notification target")

// AddTarget adds a target.
func (s *Service) AddTarget(in Target) (PublicTarget, error) {
	in.ID = ""
	t, err := in.merge(nil)
	if err != nil {
		return PublicTarget{}, err
	}
	t.ID = newID()
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	set, err := s.Settings()
	if err != nil {
		return PublicTarget{}, err
	}
	if len(set.Targets) >= 50 {
		return PublicTarget{}, errors.New("there can be at most 50 notification targets")
	}
	set.Targets = append(set.Targets, t)
	if err := s.saveSettings(set); err != nil {
		return PublicTarget{}, err
	}
	return t.public(), nil
}

// UpdateTarget changes a target. Secrets left blank keep their saved
// values.
func (s *Service) UpdateTarget(id string, in Target) (PublicTarget, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	set, err := s.Settings()
	if err != nil {
		return PublicTarget{}, err
	}
	i := set.index(id)
	if i < 0 {
		return PublicTarget{}, ErrNotFound
	}
	in.ID = id
	t, err := in.merge(&set.Targets[i])
	if err != nil {
		return PublicTarget{}, err
	}
	set.Targets[i] = t
	if err := s.saveSettings(set); err != nil {
		return PublicTarget{}, err
	}
	return t.public(), nil
}

// DeleteTarget removes a target.
func (s *Service) DeleteTarget(id string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	set, err := s.Settings()
	if err != nil {
		return err
	}
	i := set.index(id)
	if i < 0 {
		return ErrNotFound
	}
	set.Targets = slices.Delete(set.Targets, i, i+1)
	if err := s.saveSettings(set); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.held, id)
	delete(s.last, id)
	s.mu.Unlock()
	return nil
}

// SaveGeneral saves the settings that aren't about one target.
func (s *Service) SaveGeneral(baseURL string, diskPercent int) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("hlm's address must be an http:// or https:// URL, like https://hlm.example.com")
		}
		if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return errors.New("hlm's address can't have a query, fragment or user name")
		}
	}
	if diskPercent != 0 && (diskPercent < 50 || diskPercent > 99) {
		return errors.New("the disk space warning must be between 50% and 99% full")
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	set, err := s.Settings()
	if err != nil {
		return err
	}
	set.BaseURL, set.DiskPercent = baseURL, diskPercent
	return s.saveSettings(set)
}

// ForgetServer removes a deleted server from targets' server lists.
func (s *Service) ForgetServer(name string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	set, err := s.Settings()
	if err != nil {
		return err
	}
	changed := false
	for i := range set.Targets {
		if j := slices.Index(set.Targets[i].Servers, name); j >= 0 {
			set.Targets[i].Servers = slices.Delete(set.Targets[i].Servers, j, j+1)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveSettings(set)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	topicRE = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
	clockRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	// headerNameRE is an HTTP token.
	headerNameRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$")
)

// merge checks a target as sent by the API and fills in the secrets left
// blank from old, the saved target (nil for a new one).
func (in Target) merge(old *Target) (Target, error) {
	t := in
	t.Name = strings.TrimSpace(t.Name)
	t.Kind = strings.TrimSpace(t.Kind)
	if !slices.Contains(targetKinds, t.Kind) {
		return t, fmt.Errorf("unknown kind of target %q", t.Kind)
	}
	if old != nil && old.Kind != t.Kind {
		old = nil // secrets for another kind don't carry over
	}
	if t.Name == "" {
		t.Name = map[string]string{
			KindDiscord: "Discord", KindNtfy: "ntfy", KindGotify: "Gotify", KindWebhook: "Webhook", KindEmail: "Email",
		}[t.Kind]
	}
	if len(t.Name) > 64 {
		return t, errors.New("the name can be at most 64 characters")
	}

	if t.Events == nil {
		t.Events = DefaultEvents()
	}
	events := []string{}
	for _, e := range t.Events {
		if _, ok := KindByName(e); !ok || e == KindTest {
			return t, fmt.Errorf("unknown event %q", e)
		}
		if !slices.Contains(events, e) {
			events = append(events, e)
		}
	}
	t.Events = events
	t.Servers = compact(t.Servers)
	t.Groups = compact(t.Groups)
	t.QuietStart, t.QuietEnd = strings.TrimSpace(t.QuietStart), strings.TrimSpace(t.QuietEnd)
	if (t.QuietStart == "") != (t.QuietEnd == "") {
		return t, errors.New("set both the start and end of the quiet hours, or neither")
	}
	if t.QuietStart != "" && (!clockRE.MatchString(t.QuietStart) || !clockRE.MatchString(t.QuietEnd)) {
		return t, errors.New("quiet hours are times like 22:00")
	}
	if t.QuietStart != "" && t.QuietStart == t.QuietEnd {
		return t, errors.New("the quiet hours start and end at the same time")
	}

	t.URL = strings.TrimSpace(t.URL)
	if t.URL == "" && old != nil && t.urlIsSecret() {
		t.URL = old.URL
	}
	if t.Token == "" && old != nil {
		t.Token = old.Token
	}
	if t.Password == "" && old != nil {
		t.Password = old.Password
	}

	// Clear what doesn't apply to the kind, so stale secrets aren't kept.
	switch t.Kind {
	case KindDiscord, KindWebhook, KindNtfy, KindGotify:
		t.SMTPHost, t.SMTPPort, t.Security, t.Username, t.Password, t.From, t.To = "", 0, "", "", "", "", nil
		if err := checkURL(t.URL); err != nil {
			return t, err
		}
	}
	switch t.Kind {
	case KindDiscord:
		t.Topic, t.Token, t.Headers = "", "", nil
	case KindNtfy:
		t.Headers = nil
		t.Topic = strings.TrimSpace(t.Topic)
		if !topicRE.MatchString(t.Topic) {
			return t, errors.New("the ntfy topic must be 1 to 64 letters, digits, - or _")
		}
	case KindGotify:
		t.Topic, t.Headers = "", nil
		if t.Token == "" {
			return t, errors.New("enter the Gotify application token")
		}
	case KindWebhook:
		t.Topic, t.Token = "", ""
		var headers []Header
		for _, h := range t.Headers {
			h.Name = strings.TrimSpace(h.Name)
			if h.Name == "" {
				continue
			}
			if !headerNameRE.MatchString(h.Name) {
				return t, fmt.Errorf("%q isn't a valid header name", h.Name)
			}
			if strings.ContainsAny(h.Value, "\r\n") {
				return t, fmt.Errorf("the %s header's value can't contain line breaks", h.Name)
			}
			if h.Value == "" && old != nil {
				if i := slices.IndexFunc(old.Headers, func(o Header) bool { return strings.EqualFold(o.Name, h.Name) }); i >= 0 {
					h.Value = old.Headers[i].Value
				}
			}
			headers = append(headers, h)
		}
		if len(headers) > 20 {
			return t, errors.New("a webhook can have at most 20 headers")
		}
		t.Headers = headers
	case KindEmail:
		t.URL, t.Topic, t.Token, t.Headers = "", "", "", nil
		t.SMTPHost = strings.TrimSpace(t.SMTPHost)
		if t.SMTPHost == "" || strings.ContainsAny(t.SMTPHost, "/ :") {
			return t, errors.New("enter the mail server's host name, without a port")
		}
		if t.Security == "" {
			t.Security = "starttls"
		}
		if !slices.Contains([]string{"starttls", "tls", "none"}, t.Security) {
			return t, errors.New("security must be starttls, tls or none")
		}
		if t.SMTPPort == 0 {
			t.SMTPPort = map[string]int{"starttls": 587, "tls": 465, "none": 25}[t.Security]
		}
		if t.SMTPPort < 1 || t.SMTPPort > 65535 {
			return t, errors.New("the mail server's port must be between 1 and 65535")
		}
		t.Username = strings.TrimSpace(t.Username)
		if t.Username == "" {
			t.Password = ""
		}
		from, err := mail.ParseAddress(strings.TrimSpace(t.From))
		if err != nil {
			return t, errors.New("the From address isn't a valid email address")
		}
		t.From = from.String()
		var to []string
		for _, a := range t.To {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			addr, err := mail.ParseAddress(a)
			if err != nil {
				return t, fmt.Errorf("%q isn't a valid email address", a)
			}
			to = append(to, addr.Address)
		}
		if len(to) == 0 {
			return t, errors.New("enter at least one address to send to")
		}
		if len(to) > 20 {
			return t, errors.New("send to at most 20 addresses")
		}
		t.To = to
	}
	return t, nil
}

func checkURL(raw string) error {
	if raw == "" {
		return errors.New("enter the URL")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("the URL must start with http:// or https://")
	}
	return nil
}

func compact(list []string) []string {
	var out []string
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// quiet reports whether now is in t's quiet hours.
func (t Target) quiet(now time.Time) bool {
	if t.QuietStart == "" || t.QuietEnd == "" {
		return false
	}
	start, ok1 := minutes(t.QuietStart)
	end, ok2 := minutes(t.QuietEnd)
	if !ok1 || !ok2 || start == end {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end // across midnight
}

func minutes(clock string) (int, bool) {
	tm, err := time.Parse("15:04", clock)
	if err != nil {
		return 0, false
	}
	return tm.Hour()*60 + tm.Minute(), true
}
