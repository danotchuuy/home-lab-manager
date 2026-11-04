package auth

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type recorder struct{ entries []audit.Entry }

func (r *recorder) Record(e audit.Entry) { r.entries = append(r.entries, e) }

func TestSignInsAreRecorded(t *testing.T) {
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	s := New(store, false, slog.New(slog.DiscardHandler))
	now := time.Now()
	s.now = func() time.Time { return now }
	rec := &recorder{}
	s.UseAudit(rec)
	if _, err := s.CreateAccount("alice", "correct horse battery", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	login := func(password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"Alice","password":"`+password+`"}`))
		r.RemoteAddr = "192.168.1.20:5555"
		w := httptest.NewRecorder()
		s.LocalLogin(w, r)
		return w
	}
	w := login("correct horse battery")
	for range freeFailures {
		login("wrong")
	}
	login("correct horse battery") // locked out by now

	// Signing out, with the session from the first sign-in.
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	s.Logout(httptest.NewRecorder(), r)

	var got []string
	for _, e := range rec.entries {
		got = append(got, e.Action+" "+e.Actor+" "+e.Result)
		if e.IP != "192.168.1.20" && e.Action != "auth.logout" {
			t.Errorf("IP = %q", e.IP)
		}
	}
	want := []string{"auth.login alice ok"}
	for range freeFailures {
		want = append(want, "auth.login alice denied")
	}
	want = append(want, "auth.login alice denied", "auth.logout alice ok")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("recorded:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	last := rec.entries[len(rec.entries)-2]
	if !strings.Contains(last.Error, "locked") {
		t.Errorf("lockout entry = %+v", last)
	}
	if !strings.Contains(rec.entries[freeFailures].Error, "locked for") {
		t.Errorf("the failure that locked it says so: %+v", rec.entries[freeFailures])
	}
}
