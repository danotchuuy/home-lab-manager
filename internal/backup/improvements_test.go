package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeS3 is an in-memory S3 bucket: objects by key, with multipart
// uploads, listing, get, put and delete.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	parts   map[string]map[int][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Path-style: /bucket/key.
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && key == "":
		type content struct {
			Key          string
			Size         int64
			LastModified time.Time
		}
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []content
		}
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, content{k, int64(len(f.objects[k])), time.Now()})
		}
		xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprint(len(f.parts) + 1)
		f.parts[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		f.parts[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := f.parts[q.Get("uploadId")]
		var all []byte
		for i := 1; i <= len(parts); i++ {
			all = append(all, parts[i]...)
		}
		f.objects[key] = all
		io.WriteString(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func (f *fakeS3) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// improvedSetup is a backup service with a fake bucket as the primary
// storage, and another as "offsite".
func improvedSetup(t *testing.T) (*Service, *fakeServers, *fakeS3, *fakeS3) {
	t.Helper()
	primary, offsite := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}},
		&fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
	p, o := httptest.NewServer(primary), httptest.NewServer(offsite)
	t.Cleanup(p.Close)
	t.Cleanup(o.Close)
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServers{running: true, files: map[string]string{
		"data/world.sav": "the world", "data/server.log": strings.Repeat("log line\n", 50), "data/cache/x.bin": "cache",
	}}
	s := New(store, fs, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	s3set := func(url string) Settings {
		return Settings{Settings: s3.Settings{Endpoint: url, Bucket: "b", AccessKey: "k", SecretKey: "s"}, Prefix: "hlm"}
	}
	if _, err := s.Save(ctx, s3set(p.URL)); err != nil {
		t.Fatal(err)
	}
	pub, err := s.SaveTarget(ctx, Target{Name: "Off-site", Settings: s3set(o.URL), Encrypt: true})
	if err != nil {
		t.Fatal(err)
	}
	if pub.ID != "off-site" || pub.NewKey == "" || !pub.HasKey {
		t.Fatalf("off-site target = %+v", pub)
	}
	return s, fs, primary, offsite
}

func TestRetention(t *testing.T) {
	loc := time.UTC
	at := func(s string) time.Time { t, _ := time.Parse("2006-01-02 15:04", s); return t }
	// Newest first: three today, one a day for ten days, then a month ago.
	times := []time.Time{at("2026-11-05 18:00"), at("2026-11-05 12:00"), at("2026-11-05 06:00")}
	for d := 4; d >= 1; d-- {
		times = append(times, at(fmt.Sprintf("2026-11-%02d 04:00", d)))
	}
	for d := 31; d >= 26; d-- {
		times = append(times, at(fmt.Sprintf("2026-10-%02d 04:00", d)))
	}
	times = append(times, at("2026-09-15 04:00"), at("2026-08-15 04:00"))

	k := Keeping{Retention: &Retention{Last: 2, Daily: 3, Weekly: 2, Monthly: 3}}
	keep := k.keepSet(times, loc)
	var kept []string
	for i, ok := range keep {
		if ok {
			kept = append(kept, times[i].Format("01-02 15h"))
		}
	}
	// Last 2: the two newest. Daily 3: newest on Nov 5, 4, 3. Weekly 2:
	// this ISO week (Nov 2-5: Nov 5 18h) and the last (Oct 26-Nov 1: Nov 1).
	// Monthly 3: November, October (Oct 31), September.
	want := []string{"11-05 18h", "11-05 12h", "11-04 04h", "11-03 04h", "11-01 04h", "10-31 04h", "09-15 04h"}
	if !slices.Equal(kept, want) {
		t.Errorf("kept %v\nwant %v", kept, want)
	}
	// Keep N.
	if keep := (Keeping{Keep: 2}).keepSet(times, loc); !keep[1] || keep[2] {
		t.Errorf("keep 2 = %v", keep)
	}
	if keep := (Keeping{}).keepSet(times, loc); slices.Contains(keep, false) {
		t.Error("keep 0 deleted something")
	}
	for _, bad := range []Keeping{{Keep: -1}, {Retention: &Retention{}}, {Retention: &Retention{Daily: -2, Last: 1}}} {
		if bad.check() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestDueAt(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Oslo")
	at := &At{Time: "04:00", Weekdays: []int{1, 3, 5}} // Mon, Wed, Fri
	since := time.Date(2026, 11, 2, 12, 0, 0, 0, loc)  // a Monday, after 04:00
	p := Plan{Folders: []string{"/data"}, At: at, Since: since}
	if due(p, time.Date(2026, 11, 3, 23, 0, 0, 0, loc), loc) {
		t.Error("due on Tuesday")
	}
	if !due(p, time.Date(2026, 11, 4, 4, 0, 0, 0, loc), loc) {
		t.Error("not due Wednesday 04:00")
	}
	// Done Wednesday: next is Friday.
	p.Last = &Run{Started: time.Date(2026, 11, 4, 4, 0, 5, 0, loc)}
	if due(p, time.Date(2026, 11, 5, 4, 0, 0, 0, loc), loc) {
		t.Error("due Thursday")
	}
	// Failed: tried again an hour later.
	p.Last.Error = "bucket unreachable"
	if !due(p, time.Date(2026, 11, 4, 5, 1, 0, 0, loc), loc) {
		t.Error("failed backup not retried")
	}
	// Stale after twice the longest gap (Fri → Mon, 3 days).
	if d := p.staleAfter(loc, time.Date(2026, 11, 4, 5, 0, 0, 0, loc)); d != 6*24*time.Hour {
		t.Errorf("stale after %s", d)
	}
	if d := (Plan{IntervalHours: 6}).staleAfter(loc, time.Now()); d != 36*time.Hour {
		t.Errorf("6-hourly stale after %s", d)
	}
	if _, err := (At{Time: "25:00"}).check(); err == nil {
		t.Error("accepted 25:00")
	}
}

type recNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *recNotifier) Notify(e notify.Event) {
	n.mu.Lock()
	n.events = append(n.events, e)
	n.mu.Unlock()
}
func (n *recNotifier) Clear(string) {}

func TestStale(t *testing.T) {
	s, _, _, _ := improvedSetup(t)
	n := &recNotifier{}
	s.UseNotifier(n)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	plans := map[string]Plan{
		"fresh":   {Folders: []string{"/d"}, IntervalHours: 24, LastOK: &Run{Started: now.Add(-30 * time.Hour)}},
		"overdue": {Folders: []string{"/d"}, IntervalHours: 24, LastOK: &Run{Started: now.Add(-50 * time.Hour)}, Last: &Run{Error: "disk full"}},
		"manual":  {Folders: []string{"/d"}, LastOK: &Run{Started: now.Add(-1000 * time.Hour)}},
	}
	s.checkStale(plans, now)
	s.checkStale(plans, now.Add(time.Minute)) // checked once an hour
	if len(n.events) != 1 || n.events[0].Server != "overdue" || n.events[0].Kind != notify.KindBackupStale ||
		!strings.Contains(n.events[0].Message, "disk full") {
		t.Errorf("events = %+v", n.events)
	}
}

func TestVerifyCopyAndExclude(t *testing.T) {
	s, fs, primary, offsite := improvedSetup(t)
	ctx := context.Background()
	var sent []string
	s.UseConsole(func(_ context.Context, server, cmd string) error {
		sent = append(sent, server+": "+cmd)
		return nil
	})
	plan := Plan{Folders: []string{"/data"}, Keep: 2, Verify: VerifyAlways, Exclude: []string{"*.log", " cache/ "},
		PreCommand: "save-off\n\n save-all flush ", PostCommand: "save-on",
		CopyTo: &CopyPlan{Target: "off-site", Keeping: Keeping{Retention: &Retention{Last: 1}}}}
	saved, err := s.SetPlan(ctx, "srv", plan)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PreCommand != "save-off\nsave-all flush" || !slices.Equal(saved.Exclude, []string{"*.log", "cache/"}) {
		t.Errorf("saved plan = %+v", saved)
	}
	var runs []Run
	for i := 0; i < 3; i++ {
		run, err := s.BackupNow(ctx, "srv", "manual")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
		time.Sleep(1100 * time.Millisecond) // keys are by the second
	}
	run := runs[2]
	if !slices.Equal(sent[:3], []string{"srv: save-off", "srv: save-all flush", "srv: save-on"}) {
		t.Errorf("commands = %q", sent)
	}
	if run.Verify == nil || !run.Verify.OK || !run.Verify.Checksum || run.SHA256 == "" || run.Copy == nil || run.Copy.Error != "" {
		t.Fatalf("run = %+v verify %+v copy %+v", run, run.Verify, run.Copy)
	}
	// The fake archive doesn't apply patterns; files.ArchiveExcluding is
	// tested on its own. The patterns reach it:
	if s.servers.(*fakeServers) != fs {
		t.Fatal("unexpected servers")
	}

	// Kept 2 here (with their checksums), 1 off-site, encrypted.
	if keys := primary.keys("hlm/srv/"); len(keys) != 4 || !strings.HasSuffix(keys[1], ".tar.gz.sha256") {
		t.Errorf("primary = %q", keys)
	}
	off := offsite.keys("hlm/srv/")
	if len(off) != 2 {
		t.Fatalf("off-site = %q", off)
	}
	enc := offsite.objects[off[0]]
	if !isEncrypted(enc) || bytes.Contains(enc, []byte("the world")) {
		t.Error("off-site copy isn't encrypted")
	}

	// Download from the off-site copy: decrypted.
	rc, size, err := s.Download(ctx, "srv", run.Copy.Key, "off-site")
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	rc.Close()
	if size != -1 || len(names) != 3 {
		t.Errorf("downloaded %q (size %d)", names, size)
	}

	// Verify on demand, there and here.
	if v, err := s.VerifyBackup(ctx, "srv", run.Copy.Key, "off-site"); err != nil || !v.OK || v.Files != 3 {
		t.Errorf("verify off-site = %+v, %v", v, err)
	}
	// Damage the primary copy: verifying finds it.
	primary.mu.Lock()
	primary.objects[run.Key][len(primary.objects[run.Key])/2] ^= 0xff
	primary.mu.Unlock()
	n := &recNotifier{}
	s.UseNotifier(n)
	if v, err := s.VerifyBackup(ctx, "srv", run.Key, ""); err != nil || v.OK || v.Error == "" {
		t.Errorf("damaged backup verified: %+v, %v", v, err)
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindBackupVerifyFailed {
		t.Errorf("events = %+v", n.events)
	}
	o, _ := s.Overview(ctx, "srv")
	if c, ok := o.Plan.Checks["primary:"+run.Key]; !ok || c.OK {
		t.Errorf("checks = %+v", o.Plan.Checks)
	}
	if o.Copies == nil || o.Copies.Target.Name != "Off-site" || len(o.Copies.Backups) != 1 || len(o.Targets) != 1 {
		t.Errorf("overview copies = %+v targets %+v", o.Copies, o.Targets)
	}

	// Restore from the off-site copy.
	fs.files = map[string]string{}
	if err := s.Restore(ctx, "srv", run.Copy.Key, "off-site", false); err != nil {
		t.Fatal(err)
	}
	for s.Busy("srv") {
		time.Sleep(10 * time.Millisecond)
	}
	fs.mu.Lock()
	got := fs.files["data/world.sav"]
	fs.mu.Unlock()
	if got != "the world" {
		t.Errorf("restored files = %v", fs.files)
	}

	// The primary can't go while another storage exists; the key can be
	// read back for safekeeping.
	if err := s.DeleteTarget(PrimaryID); err == nil {
		t.Error("deleted the primary with another storage left")
	}
	if k, err := s.Key("off-site"); err != nil || len(k) != 44 {
		t.Errorf("key = %q, %v", k, err)
	}
	// Plans can't copy to the primary, or to storage that doesn't exist.
	for _, bad := range []*CopyPlan{{Target: PrimaryID}, {Target: "nope"}} {
		plan.CopyTo = bad
		if _, err := s.SetPlan(ctx, "srv", plan); err == nil {
			t.Errorf("accepted copying to %q", bad.Target)
		}
	}
}

func TestTargetsMigration(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	// Settings saved before targets existed.
	if err := store.Set(secretSettings, Settings{Settings: s3.Settings{Endpoint: "http://old", Bucket: "b"}, Prefix: "p"}); err != nil {
		t.Fatal(err)
	}
	s := New(store, &fakeServers{}, slog.New(slog.DiscardHandler))
	ts, err := s.Targets()
	if err != nil || len(ts) != 1 || ts[0].ID != PrimaryID || ts[0].Endpoint != "http://old" || !s.Configured() {
		t.Errorf("targets = %+v, %v", ts, err)
	}
	if err := s.Delete(); err != nil || s.Configured() {
		t.Errorf("delete: %v, configured %v", err, s.Configured())
	}
}
