package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// recorder is a webhook endpoint that records what it receives.
type recorder struct {
	mu       sync.Mutex
	requests []recorded
	// fail is how many requests to answer with status before succeeding.
	fail   int
	status int
}

type recorded struct {
	path    string
	headers http.Header
	body    map[string]any
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	rc.mu.Lock()
	rc.requests = append(rc.requests, recorded{r.URL.Path, r.Header.Clone(), body})
	fail := rc.fail > 0
	if fail {
		rc.fail--
	}
	rc.mu.Unlock()
	if fail {
		http.Error(w, "nope", rc.status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (rc *recorder) all() []recorded {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.requests)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newService(t *testing.T, o Options) (*Service, *clock) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "test key")
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)}
	o.Secrets = store
	if o.StatePath == "" {
		o.StatePath = filepath.Join(t.TempDir(), "notifications.json")
	}
	o.Now = clk.Now
	o.RetryWait = func(int) time.Duration { return time.Millisecond }
	return New(o), clk
}

// send reports e and handles it straight away.
func send(s *Service, e Event) {
	s.Notify(e)
	s.handle(context.Background(), <-s.queue)
}

func addTarget(t *testing.T, s *Service, in Target) PublicTarget {
	t.Helper()
	p, err := s.AddTarget(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscordAndWebhookBodies(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, _ := newService(t, Options{BaseURL: func() string { return "https://hlm.example.com" }})
	addTarget(t, s, Target{Kind: KindDiscord, Enabled: true, URL: srv.URL + "/api/webhooks/1/tok"})
	addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: srv.URL + "/hook",
		Headers: []Header{{Name: "X-Secret", Value: "s3cret"}}})

	send(s, Event{Kind: KindBackupFailed, Title: "Backup of valheim failed", Message: "upload: refused",
		Server: "valheim", Link: "/servers/valheim?tab=backups", Fields: map[string]string{"Trigger": "schedule"}})

	reqs := rc.all()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
	d := reqs[0]
	embed := d.body["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "Backup of valheim failed" || embed["url"] != "https://hlm.example.com/servers/valheim?tab=backups" ||
		embed["color"].(float64) != float64(discordColor[Error]) {
		t.Errorf("discord embed = %v", embed)
	}
	w := reqs[1]
	if w.path != "/hook" || w.headers.Get("X-Secret") != "s3cret" {
		t.Errorf("webhook request = %s %v", w.path, w.headers)
	}
	if w.body["kind"] != KindBackupFailed || w.body["severity"] != "error" || w.body["server"] != "valheim" ||
		w.body["link"] != "https://hlm.example.com/servers/valheim?tab=backups" ||
		w.body["fields"].(map[string]any)["Trigger"] != "schedule" {
		t.Errorf("webhook body = %v", w.body)
	}
	h := s.History()
	if len(h) != 1 || len(h[0].Deliveries) != 2 || !h[0].Deliveries[0].OK || !h[0].Deliveries[1].OK {
		t.Errorf("history = %+v", h)
	}
}

func TestNtfyAndGotify(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, _ := newService(t, Options{})
	addTarget(t, s, Target{Kind: KindNtfy, Enabled: true, URL: srv.URL, Topic: "homelab", Token: "tk_abc"})
	addTarget(t, s, Target{Kind: KindGotify, Enabled: true, URL: srv.URL + "/gotify/", Token: "AppTok"})
	send(s, Event{Kind: KindServerCrashed, Title: "valheim crashed", Message: "exit code 1", Server: "valheim"})

	reqs := rc.all()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests", len(reqs))
	}
	n := reqs[0]
	if n.path != "/" || n.headers.Get("Authorization") != "Bearer tk_abc" || n.body["topic"] != "homelab" ||
		n.body["priority"].(float64) != 5 || !strings.Contains(n.body["message"].(string), "Server: valheim") {
		t.Errorf("ntfy = %s %v %v", n.path, n.headers, n.body)
	}
	g := reqs[1]
	if g.path != "/gotify/message" || g.headers.Get("X-Gotify-Key") != "AppTok" || g.body["priority"].(float64) != 8 {
		t.Errorf("gotify = %s %v %v", g.path, g.headers, g.body)
	}
}

func TestRetries(t *testing.T) {
	rc := &recorder{fail: 2, status: http.StatusBadGateway}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, _ := newService(t, Options{})
	p := addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: srv.URL})
	send(s, Event{Kind: KindServerUpdateFail, Title: "x"})
	if n := len(rc.all()); n != 3 {
		t.Errorf("sent %d times, want 3 (two failures, then success)", n)
	}
	pub, _ := s.Public()
	if pub.Targets[0].Last == nil || !pub.Targets[0].Last.OK || pub.Targets[0].ID != p.ID {
		t.Errorf("last delivery = %+v", pub.Targets[0].Last)
	}

	// A 4xx means the target refuses it; no point retrying.
	rc2 := &recorder{fail: 5, status: http.StatusUnauthorized}
	srv2 := httptest.NewServer(rc2)
	defer srv2.Close()
	if _, err := s.UpdateTarget(p.ID, Target{Kind: KindWebhook, Enabled: true, URL: srv2.URL, Events: []string{KindServerUpdateFail}}); err != nil {
		t.Fatal(err)
	}
	send(s, Event{Kind: KindServerUpdateFail, Title: "y"})
	if n := len(rc2.all()); n != 1 {
		t.Errorf("sent %d times after a 401, want 1", n)
	}
	pub, _ = s.Public()
	if last := pub.Targets[0].Last; last == nil || last.OK || !strings.Contains(last.Error, "401") {
		t.Errorf("last delivery = %+v", last)
	}
}

func TestRepeatsAreHeldBack(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	statePath := filepath.Join(t.TempDir(), "state.json")
	s, clk := newService(t, Options{StatePath: statePath})
	addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: srv.URL,
		Events: []string{KindServerCrashed, KindBackupFailed, KindUpdateAvailable, KindServerUpdated}})

	crash := Event{Kind: KindServerCrashed, Title: "crashed", Server: "valheim"}
	send(s, crash)
	send(s, crash) // a repeat within 15 minutes
	send(s, Event{Kind: KindServerCrashed, Title: "crashed", Server: "other"})
	clk.add(16 * time.Minute)
	send(s, crash)
	if n := len(rc.all()); n != 3 {
		t.Errorf("crashes sent %d times, want 3", n)
	}
	if h := s.History(); !h[2].Suppressed {
		t.Errorf("repeat wasn't marked suppressed: %+v", h[2])
	}

	// Clear lets the next failure through at once.
	fail := Event{Kind: KindBackupFailed, Title: "backup failed", Server: "valheim"}
	send(s, fail)
	send(s, fail)
	s.Clear(DedupeKey(KindBackupFailed, "valheim"))
	send(s, fail)
	if n := len(rc.all()); n != 5 {
		t.Errorf("sent %d, want 5 after clearing", n)
	}

	// Sent-once kinds are remembered across restarts.
	upd := Event{Kind: KindUpdateAvailable, Title: "update", Server: "valheim", Key: "update/valheim/sha256:1"}
	send(s, upd)
	clk.add(30 * 24 * time.Hour)
	send(s, upd)
	s2 := New(Options{Secrets: s.opts.Secrets, StatePath: statePath, Now: clk.Now})
	send(s2, upd)
	if n := len(rc.all()); n != 6 {
		t.Errorf("sent-once event sent %d times in total, want 6", n)
	}
	// Kinds without a repeat window are always sent.
	send(s2, Event{Kind: KindServerUpdated, Title: "updated", Server: "valheim"})
	send(s2, Event{Kind: KindServerUpdated, Title: "updated", Server: "valheim"})
	if n := len(rc.all()); n != 8 {
		t.Errorf("sent %d, want 8", n)
	}
	if h := s2.History(); len(h) != 12 {
		t.Errorf("history has %d events after a restart, want 12", len(h))
	}
}

func TestFilters(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, _ := newService(t, Options{GroupServers: func(id string) []string {
		if id == "g1" {
			return []string{"minecraft"}
		}
		return nil
	}})
	addTarget(t, s, Target{Kind: KindWebhook, Name: "some", Enabled: true, URL: srv.URL + "/some",
		Events: []string{KindServerUpdated, KindDiskLow}, Servers: []string{"valheim"}, Groups: []string{"g1"}})
	addTarget(t, s, Target{Kind: KindWebhook, Name: "off", Enabled: false, URL: srv.URL + "/off", Events: []string{KindServerUpdated}})
	addTarget(t, s, Target{Kind: KindWebhook, Name: "default", Enabled: true, URL: srv.URL + "/default"})

	send(s, Event{Kind: KindServerUpdated, Title: "a", Server: "valheim"})   // some
	send(s, Event{Kind: KindServerUpdated, Title: "b", Server: "minecraft"}) // some, by group
	send(s, Event{Kind: KindServerUpdated, Title: "c", Server: "plex"})      // nobody
	send(s, Event{Kind: KindDiskLow, Title: "d"})                            // some, default (not about a server)
	send(s, Event{Kind: KindServerCrashed, Title: "e", Server: "plex"})      // default (warnings and errors)

	var got []string
	for _, r := range rc.all() {
		got = append(got, r.path+":"+r.body["title"].(string))
	}
	want := []string{"/some:a", "/some:b", "/some:d", "/default:d", "/default:e"}
	if !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestQuietHours(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, clk := newService(t, Options{})
	clk.now = time.Date(2026, 11, 4, 23, 30, 0, 0, time.Local)
	addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: srv.URL, QuietStart: "22:00", QuietEnd: "07:00",
		Events: []string{KindServerUpdated, KindServerCrashed}})

	send(s, Event{Kind: KindServerUpdated, Title: "held"})
	send(s, Event{Kind: KindServerCrashed, Title: "urgent", Server: "x"})
	if r := rc.all(); len(r) != 1 || r[0].body["title"] != "urgent" {
		t.Fatalf("during quiet hours sent %v, want only the error", r)
	}
	if h := s.History(); !h[1].Deliveries[0].Held {
		t.Errorf("held event not recorded as held: %+v", h[1])
	}
	s.releaseHeld(context.Background())
	if len(rc.all()) != 1 {
		t.Error("held event sent before the quiet hours ended")
	}
	clk.now = time.Date(2026, 11, 5, 7, 1, 0, 0, time.Local)
	s.releaseHeld(context.Background())
	if r := rc.all(); len(r) != 2 || r[1].body["title"] != "held" {
		t.Errorf("after quiet hours sent %v", r)
	}
}

func TestQuiet(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	night := Target{QuietStart: "22:00", QuietEnd: "07:00"}
	day := Target{QuietStart: "09:00", QuietEnd: "17:30"}
	tests := []struct {
		t    Target
		at   time.Time
		want bool
	}{
		{night, at(23, 0), true}, {night, at(3, 0), true}, {night, at(7, 0), false}, {night, at(21, 59), false},
		{day, at(9, 0), true}, {day, at(17, 29), true}, {day, at(17, 30), false}, {day, at(8, 0), false},
		{Target{}, at(3, 0), false},
	}
	for _, tt := range tests {
		if got := tt.t.quiet(tt.at); got != tt.want {
			t.Errorf("%s-%s at %s: quiet = %v", tt.t.QuietStart, tt.t.QuietEnd, tt.at.Format("15:04"), got)
		}
	}
}

func TestSecretsAreKeptAndHidden(t *testing.T) {
	s, _ := newService(t, Options{})
	p := addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: "https://hooks.example.com/abc?token=xyz",
		Headers: []Header{{Name: "Authorization", Value: "Bearer secret-token"}}})
	if p.URL != "" || p.URLHint != "https://hooks.example.com/…" || !p.HasURL || p.Headers[0].Value != "" {
		t.Errorf("public target shows secrets: %+v", p)
	}
	pub, _ := s.Public()
	data, _ := json.Marshal(pub)
	for _, secret := range []string{"xyz", "secret-token", "/abc"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("settings response contains %q: %s", secret, data)
		}
	}

	// Blank secrets keep their saved values.
	if _, err := s.UpdateTarget(p.ID, Target{Kind: KindWebhook, Name: "renamed", Enabled: true,
		Headers: []Header{{Name: "Authorization"}}}); err != nil {
		t.Fatal(err)
	}
	set, _ := s.Settings()
	got := set.Targets[0]
	if got.Name != "renamed" || got.URL != "https://hooks.example.com/abc?token=xyz" || got.Headers[0].Value != "Bearer secret-token" {
		t.Errorf("after an update without secrets: %+v", got)
	}

	// Changing the kind doesn't carry secrets over.
	if _, err := s.UpdateTarget(p.ID, Target{Kind: KindDiscord, Enabled: true}); err == nil {
		t.Error("changing to Discord without a URL was accepted (it kept the webhook's URL)")
	}

	if _, err := s.UpdateTarget("nope", Target{Kind: KindWebhook}); err != ErrNotFound {
		t.Errorf("updating a missing target: %v", err)
	}
	if err := s.DeleteTarget(p.ID); err != nil {
		t.Fatal(err)
	}
	if pub, _ := s.Public(); len(pub.Targets) != 0 {
		t.Errorf("target not deleted: %+v", pub.Targets)
	}
}

func TestValidation(t *testing.T) {
	bad := []Target{
		{Kind: "pager"},
		{Kind: KindDiscord, URL: "ftp://x"},
		{Kind: KindDiscord},
		{Kind: KindNtfy, URL: "https://ntfy.sh", Topic: "bad topic"},
		{Kind: KindGotify, URL: "https://gotify.example.com"},
		{Kind: KindWebhook, URL: "https://x", Headers: []Header{{Name: "Bad Name", Value: "v"}}},
		{Kind: KindWebhook, URL: "https://x", Headers: []Header{{Name: "X", Value: "a\r\nInjected: 1"}}},
		{Kind: KindWebhook, URL: "https://x", Events: []string{"made.up"}},
		{Kind: KindWebhook, URL: "https://x", QuietStart: "22:00"},
		{Kind: KindWebhook, URL: "https://x", QuietStart: "25:00", QuietEnd: "07:00"},
		{Kind: KindEmail, SMTPHost: "smtp.example.com", From: "hlm@example.com"},
		{Kind: KindEmail, SMTPHost: "smtp.example.com:587", From: "hlm@example.com", To: []string{"a@example.com"}},
		{Kind: KindEmail, SMTPHost: "smtp.example.com", From: "not an address", To: []string{"a@example.com"}},
		{Kind: KindEmail, SMTPHost: "smtp.example.com", From: "hlm@example.com", To: []string{"a@example.com"}, Security: "ssl"},
	}
	for _, in := range bad {
		if _, err := in.merge(nil); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	e, err := Target{Kind: KindEmail, SMTPHost: "smtp.example.com", From: "hlm <hlm@example.com>",
		To: []string{" a@example.com ", ""}, Security: "tls", Token: "dropped"}.merge(nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.SMTPPort != 465 || len(e.To) != 1 || e.Token != "" || e.Name != "Email" || !slices.Equal(e.Events, DefaultEvents()) {
		t.Errorf("email target = %+v", e)
	}
}

func TestGeneralSettings(t *testing.T) {
	s, _ := newService(t, Options{BaseURL: func() string { return "https://hlm.example.com" }})
	for _, bad := range []string{"hlm.example.com", "ftp://x", "https://u:p@x"} {
		if err := s.SaveGeneral(bad, 0); err == nil {
			t.Errorf("accepted address %q", bad)
		}
	}
	if err := s.SaveGeneral("https://x", 30); err == nil {
		t.Error("accepted a 30% disk warning")
	}
	if err := s.SaveGeneral(" http://192.168.1.10:8080/ ", 95); err != nil {
		t.Fatal(err)
	}
	pub, _ := s.Public()
	if pub.BaseURL != "http://192.168.1.10:8080" || pub.DiskPercent != 95 || pub.DefaultBaseURL != "https://hlm.example.com" {
		t.Errorf("public settings = %+v", pub)
	}
	set, _ := s.Settings()
	if m := s.message(set, Event{Link: "/host"}); m.URL != "http://192.168.1.10:8080/host" {
		t.Errorf("link = %q", m.URL)
	}
}

func TestForgetServer(t *testing.T) {
	s, _ := newService(t, Options{})
	addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: "https://x", Servers: []string{"a", "b"}})
	if err := s.ForgetServer("a"); err != nil {
		t.Fatal(err)
	}
	set, _ := s.Settings()
	if !slices.Equal(set.Targets[0].Servers, []string{"b"}) {
		t.Errorf("servers = %v", set.Targets[0].Servers)
	}
}

func TestRedact(t *testing.T) {
	tests := map[string]string{
		"dial https://user:hunter2@s3.lan:8333/bucket":                                     "dial https://***@s3.lan:8333/bucket",
		"GET /x?token=abc123&page=2":                                                       "GET /x?token=***&page=2",
		"X-Amz-Signature=deadbeef failed":                                                  "X-Amz-Signature=*** failed",
		"Authorization: Bearer abcdefghijkl":                                               "Authorization: Bearer ***",
		"https://discord.com/api/webhooks/123/AbC-dEf":                                     "https://discord.com/api/webhooks/123/***",
		"POST /api/v10/interactions/123/aW50ZXJhY3Rpb24.x/callback":                        "POST /api/v10/interactions/123/***/callback",
		"PATCH /api/v10/webhooks/123/aW50ZXJh/messages/@original":                          "PATCH /api/v10/webhooks/123/***/messages/@original",
		"Authorization: Bot MTAwMDAwMDAwMDAwMDAwMDAy.GaBcDe.abcdefghijklmnopqrstuvwxyz012": "Authorization: Bot ***",
		"token MTAwMDAwMDAwMDAwMDAwMDAy.GaBcDe.abcdefghijklmnopqrstuvwxyz012 leaked":       "token *** leaked",
		"exit code 1":      "exit code 1",
		"server 1.21.4 up": "server 1.21.4 up",
	}
	for in, want := range tests {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMessagesAreRedacted(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, _ := newService(t, Options{})
	addTarget(t, s, Target{Kind: KindWebhook, Enabled: true, URL: srv.URL})
	send(s, Event{Kind: KindBackupFailed, Title: "Backup failed", Message: "PUT http://key:secretkey@s3.lan/b failed"})
	if msg := rc.all()[0].body["message"].(string); strings.Contains(msg, "secretkey") {
		t.Errorf("message not redacted: %q", msg)
	}
}

func TestQueueNeverBlocks(t *testing.T) {
	s, _ := newService(t, Options{})
	done := make(chan struct{})
	go func() {
		for range queueSize + 10 {
			s.Notify(Event{Kind: KindServerUpdated})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked on a full queue")
	}
	if pub, _ := s.Public(); pub.Dropped != 10 {
		t.Errorf("dropped = %d, want 10", pub.Dropped)
	}
	var nilService *Service
	nilService.Notify(Event{}) // must not panic
	nilService.Clear("x")
}

func TestTestMessage(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, _ := newService(t, Options{})
	p := addTarget(t, s, Target{Kind: KindWebhook, Name: "Hook", Enabled: true, URL: srv.URL + "/saved"})
	// Testing a saved target with its URL left blank uses the saved URL.
	if err := s.Test(context.Background(), Target{ID: p.ID, Kind: KindWebhook, Name: "Hook"}); err != nil {
		t.Fatal(err)
	}
	if r := rc.all(); len(r) != 1 || r[0].path != "/saved" || r[0].body["kind"] != KindTest {
		t.Errorf("test sent %v", r)
	}
	rc.fail, rc.status = 1, http.StatusForbidden
	if err := s.Test(context.Background(), Target{ID: p.ID, Kind: KindWebhook}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("test against a refusing target: %v", err)
	}
}
