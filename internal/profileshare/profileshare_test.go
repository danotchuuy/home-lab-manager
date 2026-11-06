package profileshare

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const mygame = `id: mygame
name: My Game
match: [[mygame]]
`

// site serves files over HTTPS.
type site struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
}

func newSite(t *testing.T) *site {
	s := &site{files: map[string]string{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		body, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/slow":
			time.Sleep(fetchTimeout + time.Second)
		case strings.HasPrefix(r.URL.Path, "/redirect-http"):
			http.Redirect(w, r, "http://example.com/x.yaml", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/redirect-loop"):
			http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
		case !ok:
			http.NotFound(w, r)
		default:
			io.WriteString(w, body)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *site) put(p, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[p] = body
}

func setup(t *testing.T) (*Service, *site, *gameprofile.Store) {
	t.Helper()
	srv := newSite(t)
	dir := t.TempDir()
	store := gameprofile.NewStore(filepath.Join(dir, "game-profiles"))
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gameprofile.Use(nil) })
	sec, err := secrets.Open(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	tr := srv.Client().Transport.(*http.Transport)
	f := &Fetcher{Transport: tr, UserAgent: "hlm-test", Allow: func(netip.Addr) bool { return true }}
	return New(Options{Store: store, Secrets: sec, Fetcher: f}), srv, store
}

func TestFetchLimits(t *testing.T) {
	s, srv, _ := setup(t)
	ctx := context.Background()
	srv.put("/ok.yaml", mygame)
	srv.put("/big.yaml", mygame+"# "+strings.Repeat("x", gameprofile.MaxSize))
	srv.put("/notyaml.yaml", "<html>hello</html>")
	srv.put("/invalid.yaml", "id: Bad ID\nname: x\nmatch: [[x]]\n")
	if _, err := s.Preview(ctx, srv.URL+"/ok.yaml"); err != nil {
		t.Fatalf("ok: %v", err)
	}
	for path, want := range map[string]string{
		"/big.yaml":      "larger than",
		"/notyaml.yaml":  "isn't a game profile",
		"/invalid.yaml":  "isn't a game profile",
		"/missing.yaml":  "HTTP 404",
		"/redirect-http": "isn't HTTPS",
		"/redirect-loop": "redirects",
	} {
		if _, err := s.Preview(ctx, srv.URL+path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
	for link, want := range map[string]string{
		"http://example.com/x.yaml":            "HTTPS",
		"https://user:pw@example.com/x":        "user name",
		"not a link":                           "isn't a link",
		"ftp://example.com/x.yaml":             "HTTPS",
		"https://" + strings.Repeat("a", 2100): "too long",
	} {
		if _, err := s.Preview(ctx, link); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", link, err, want)
		}
	}
}

func TestSlow(t *testing.T) {
	if testing.Short() {
		t.Skip("takes the fetch timeout")
	}
	s, srv, _ := setup(t)
	start := time.Now()
	if _, err := s.Preview(context.Background(), srv.URL+"/slow"); err == nil {
		t.Fatal("slow fetch worked")
	}
	if time.Since(start) > fetchTimeout+3*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

// The real address check refuses this machine's own addresses, also when
// a name resolves to one.
func TestBlockedAddresses(t *testing.T) {
	srv := newSite(t)
	srv.put("/ok.yaml", mygame)
	f := &Fetcher{Transport: srv.Client().Transport.(*http.Transport), UserAgent: "hlm-test"}
	_, _, err := f.Profile(context.Background(), srv.URL+"/ok.yaml")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("127.0.0.1: %v", err)
	}
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	_, _, err = f.Profile(context.Background(), "https://localhost"+port+"/ok.yaml")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("localhost: %v", err)
	}
	for addr, ok := range map[string]bool{
		"127.0.0.1": false, "169.254.169.254": false, "::1": false, "fe80::1": false, "fd00:ec2::254": false,
		"::ffff:127.0.0.1": false, "0.0.0.0": false, "224.0.0.1": false,
		"192.168.1.10": true, "10.0.0.5": true, "140.82.112.3": true, "2606:50c0:8000::154": true,
	} {
		if Allowed(netip.MustParseAddr(addr)) != ok {
			t.Errorf("%s: allowed %v", addr, !ok)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/me/profiles/blob/main/games/mygame.yaml": "https://raw.githubusercontent.com/me/profiles/main/games/mygame.yaml",
		"https://gist.github.com/me/0123abcd":                        "https://gist.githubusercontent.com/me/0123abcd/raw",
		"  https://example.com/x.yaml#top ":                          "https://example.com/x.yaml",
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("%s: %s, %v", in, got, err)
		}
	}
}

func TestImportAndUpdates(t *testing.T) {
	s, srv, store := setup(t)
	ctx := context.Background()
	link := srv.URL + "/mygame.yaml"
	srv.put("/mygame.yaml", mygame)

	pv, err := s.Preview(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if pv.ID != "mygame" || pv.Replaces != "" || pv.SHA256 != gameprofile.Hash([]byte(mygame)) {
		t.Fatalf("preview: %+v", pv)
	}
	// It changes between looking and saving: refused.
	srv.put("/mygame.yaml", mygame+"# changed\n")
	if _, err := s.Import(ctx, link, pv.SHA256); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed: %v", err)
	}
	srv.put("/mygame.yaml", mygame)
	p, err := s.Import(ctx, link, pv.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if p.Origin == nil || p.Origin.URL != link || p.Origin.ChangedLocally {
		t.Fatalf("imported: %+v", p.Origin)
	}

	// Nothing new.
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if o := origin(t, store, "mygame"); o.Update != "" || o.Checked.IsZero() {
		t.Errorf("unchanged: %+v", o)
	}

	// The link changes: offered, not applied.
	v2 := strings.Replace(mygame, "My Game", "My Game 2", 1)
	srv.put("/mygame.yaml", v2)
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if o := origin(t, store, "mygame"); o.Update != gameprofile.Hash([]byte(v2)) {
		t.Errorf("update: %+v", o)
	}
	if gameprofile.Current().ByID("mygame").Name != "My Game" {
		t.Error("the update was applied by itself")
	}

	// Edited by hand: the origin stays, marked changed locally.
	local := mygame + "# my note\n"
	if _, err := store.Import([]byte(local)); err != nil {
		t.Fatal(err)
	}
	if o := origin(t, store, "mygame"); !o.ChangedLocally || o.URL != link {
		t.Errorf("local change: %+v", o)
	}
	// The update's difference is from what's in use, local note included.
	pv, err = s.Preview(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Replaces != gameprofile.Custom || !hasLine(pv.Diff, "-", "# my note") || !hasLine(pv.Diff, "+", "name: My Game 2") {
		t.Errorf("update diff: %+v", pv.Diff)
	}
	if _, err := s.Import(ctx, link, pv.SHA256); err != nil {
		t.Fatal(err)
	}
	if o := origin(t, store, "mygame"); o.Update != "" || o.ChangedLocally || gameprofile.Current().ByID("mygame").Name != "My Game 2" {
		t.Errorf("accepted: %+v", o)
	}

	// A link that's gone: noted, the profile kept.
	srv.put("/mygame.yaml", "")
	delete(srv.files, "/mygame.yaml")
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if o := origin(t, store, "mygame"); !strings.Contains(o.UpdateError, "404") {
		t.Errorf("gone: %+v", o)
	}

	// Deleting the profile forgets where it came from.
	if err := store.Delete("mygame"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), ".origins.json")); !os.IsNotExist(err) {
		t.Errorf("origins file left: %v", err)
	}
}

func TestReplacesBuiltin(t *testing.T) {
	s, srv, _ := setup(t)
	mc, _ := gameprofile.Builtin("minecraft")
	srv.put("/mc.yaml", string(mc)+"# tweaked\n")
	pv, err := s.Preview(context.Background(), srv.URL+"/mc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Replaces != gameprofile.BuiltIn || pv.Same || !hasLine(pv.Diff, "+", "# tweaked") {
		t.Errorf("preview: replaces %q, same %v", pv.Replaces, pv.Same)
	}
	for _, l := range pv.Diff {
		if l.Op == "-" {
			t.Errorf("removed line: %q", l.Text)
		}
	}
}

func TestIndex(t *testing.T) {
	s, srv, store := setup(t)
	ctx := context.Background()
	srv.put("/profiles/mygame.yaml", mygame)
	other := strings.ReplaceAll(mygame, "mygame", "other")
	srv.put("/profiles/other.yaml", other)
	index := func(mySHA string) string {
		return fmt.Sprintf(`name: Community
profiles:
  - id: mygame
    name: My Game
    url: profiles/mygame.yaml
    sha256: %s
  - id: other
    name: Other
    url: %s/profiles/other.yaml
    sha256: %s
    description: Another game
`, mySHA, srv.URL, gameprofile.Hash([]byte(other)))
	}
	srv.put("/index.yaml", index(gameprofile.Hash([]byte(mygame))))
	ixURL := srv.URL + "/index.yaml"

	if _, err := s.PreviewIndexed(ctx, ixURL, "mygame"); err == nil || !strings.Contains(err.Error(), "add it first") {
		t.Errorf("unlisted index: %v", err)
	}
	if _, err := s.SetIndexes([]string{ixURL, ixURL}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Indexes(); len(got) != 1 {
		t.Errorf("indexes: %v", got)
	}
	ls, err := s.Browse(ctx)
	if err != nil || len(ls) != 1 || ls[0].Name != "Community" || len(ls[0].Profiles) != 2 ||
		ls[0].Profiles[0].URL != srv.URL+"/profiles/mygame.yaml" {
		t.Fatalf("browse: %+v, %v", ls, err)
	}
	pv, err := s.PreviewIndexed(ctx, ixURL, "mygame")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportIndexed(ctx, ixURL, "mygame", pv.SHA256); err != nil {
		t.Fatal(err)
	}
	ls, _ = s.Browse(ctx)
	if e := ls[0].Profiles[0]; !e.Installed || !e.Current || e.InUse != gameprofile.Custom {
		t.Errorf("installed: %+v", e)
	}

	// The index lists a new version: an update. The file doesn't match
	// the listed hash yet: refused.
	v2 := mygame + "# v2\n"
	srv.put("/index.yaml", index(gameprofile.Hash([]byte(v2))))
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if o := origin(t, store, "mygame"); o.Update != gameprofile.Hash([]byte(v2)) || o.Index != srv.URL+"/index.yaml" {
		t.Errorf("index update: %+v", o)
	}
	if _, err := s.PreviewIndexed(ctx, ixURL, "mygame"); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Errorf("hash mismatch: %v", err)
	}
	srv.put("/profiles/mygame.yaml", v2)
	pv, err = s.PreviewIndexed(ctx, ixURL, "mygame")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportIndexed(ctx, ixURL, "mygame", pv.SHA256); err != nil {
		t.Fatal(err)
	}

	srv.put("/bad-index.yaml", "profiles:\n  - id: x\n    url: a.yaml\n    sha256: nothex\n")
	if _, err := s.opts.Fetcher.Index(ctx, srv.URL+"/bad-index.yaml"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("bad index: %v", err)
	}
}

func TestExport(t *testing.T) {
	s, _, store := setup(t)
	if _, err := store.Import([]byte(mygame)); err != nil {
		t.Fatal(err)
	}
	other := strings.ReplaceAll(mygame, "mygame", "other")
	if _, err := store.Import([]byte(other)); err != nil {
		t.Fatal(err)
	}
	data, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "mygame.yaml" && string(b) != mygame {
			t.Errorf("content: %q", b)
		}
	}
	if strings.Join(names, ",") != "mygame.yaml,other.yaml" {
		t.Errorf("files: %v", names)
	}
}

func TestDiff(t *testing.T) {
	d := Diff("a\nb\nc\n", "a\nc\nd\n")
	var got []string
	for _, l := range d {
		got = append(got, l.Op+l.Text)
	}
	if strings.Join(got, ",") != " a,-b, c,+d" {
		t.Errorf("diff: %v", got)
	}
	if Changed(Diff("x\r\n", "x\n")) {
		t.Error("line endings count as a change")
	}
}

func origin(t *testing.T, store *gameprofile.Store, id string) gameprofile.OriginInfo {
	t.Helper()
	for _, p := range store.State().Profiles {
		if p.ID == id && p.Origin != nil {
			return *p.Origin
		}
	}
	t.Fatalf("%s has no origin", id)
	return gameprofile.OriginInfo{}
}

func hasLine(d []Line, op, text string) bool {
	for _, l := range d {
		if l.Op == op && l.Text == text {
			return true
		}
	}
	return false
}
