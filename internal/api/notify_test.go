package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestNotificationEndpoints(t *testing.T) {
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	var received []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		received = append(received, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	svc := notify.New(notify.Options{Secrets: store, StatePath: filepath.Join(t.TempDir(), "n.json")})
	s := &server{Options{Notify: svc, Log: slog.New(slog.DiscardHandler)}}

	do := func(h http.HandlerFunc, method, path, body string, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if id != "" {
			r.SetPathValue("id", id)
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}

	w := do(s.addNotificationTarget, "POST", "/api/notifications/targets",
		`{"kind":"webhook","name":"Hook","enabled":true,"url":"`+hook.URL+`/secret-path"}`, "")
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "secret-path") {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	var added notify.PublicTarget
	_ = json.Unmarshal(w.Body.Bytes(), &added)

	if w := do(s.addNotificationTarget, "POST", "/", `{"kind":"pager"}`, ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad target = %d", w.Code)
	}
	if w := do(s.updateNotificationTarget, "PUT", "/", `{"kind":"webhook","url":"https://x"}`, "nope"); w.Code != http.StatusNotFound {
		t.Errorf("update missing = %d", w.Code)
	}
	if w := do(s.updateNotificationTarget, "PUT", "/", `{"kind":"webhook","name":"Renamed","enabled":true}`, added.ID); w.Code != http.StatusOK {
		t.Errorf("update = %d %s", w.Code, w.Body)
	}

	// Testing the saved target with its URL left blank reaches the saved URL.
	if w := do(s.testNotificationTarget, "POST", "/", `{"id":"`+added.ID+`","kind":"webhook","name":"Renamed"}`, ""); w.Code != http.StatusNoContent {
		t.Errorf("test = %d %s", w.Code, w.Body)
	}
	if len(received) != 1 || received[0]["kind"] != notify.KindTest {
		t.Errorf("hook received %v", received)
	}

	if w := do(s.saveNotificationSettings, "PUT", "/", `{"baseUrl":"not a url"}`, ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad base URL = %d", w.Code)
	}
	w = do(s.saveNotificationSettings, "PUT", "/", `{"baseUrl":"https://hlm.lan","diskPercent":85}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("save settings = %d %s", w.Code, w.Body)
	}
	var pub notify.PublicSettings
	_ = json.Unmarshal(w.Body.Bytes(), &pub)
	if pub.BaseURL != "https://hlm.lan" || pub.DiskPercent != 85 || len(pub.Targets) != 1 || pub.Targets[0].Name != "Renamed" ||
		!pub.Targets[0].HasURL || len(pub.Kinds) == 0 {
		t.Errorf("settings = %+v", pub)
	}
	if strings.Contains(w.Body.String(), "secret-path") {
		t.Errorf("settings show the webhook URL: %s", w.Body)
	}

	if w := do(s.notificationHistory, "GET", "/", "", ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("history = %d %s", w.Code, w.Body)
	}
	if w := do(s.deleteNotificationTarget, "DELETE", "/", "", added.ID); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d", w.Code)
	}
	if w := do(s.deleteNotificationTarget, "DELETE", "/", "", added.ID); w.Code != http.StatusNotFound {
		t.Errorf("delete again = %d", w.Code)
	}
}
