package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

func TestGameProfileEndpoints(t *testing.T) {
	t.Cleanup(func() { gameprofile.Use(nil) })
	s := &server{Options{GameProfiles: gameprofile.NewStore(filepath.Join(t.TempDir(), "game-profiles"))}}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		if strings.Contains(rt.pattern, "/api/game-profiles") {
			mux.HandleFunc(rt.pattern, rt.handler)
		}
	}
	do := func(method, path, body string) (int, string) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}

	if code, body := do("GET", "/api/game-profiles", ""); code != 200 || !strings.Contains(body, `"id":"palworld"`) ||
		!strings.Contains(body, `"source":"built-in"`) || !strings.Contains(body, `"errors":[]`) {
		t.Fatalf("list: %d %s", code, body)
	}
	code, src := do("GET", "/api/game-profiles/valheim/yaml", "")
	if code != 200 || !strings.HasPrefix(src, "id: valheim") {
		t.Fatalf("yaml: %d %s", code, src)
	}
	mine := strings.Replace(src, "name: Valheim", "name: Valheim (mine)", 1)
	if code, body := do("POST", "/api/game-profiles", mine); code != 200 || !strings.Contains(body, `"replaces":true`) ||
		!strings.Contains(body, `"source":"custom"`) {
		t.Fatalf("import: %d %s", code, body)
	}
	if code, body := do("GET", "/api/game-profiles/valheim/yaml?builtin=1", ""); code != 200 || body != src {
		t.Errorf("built-in yaml: %d %s", code, body)
	}
	if code, body := do("POST", "/api/game-profiles", "id: valheim\nname: V\nmatch: [[valheim]]\nport: 1\n"); code != 400 ||
		!strings.Contains(body, "port") {
		t.Errorf("bad import: %d %s", code, body)
	}
	if code, body := do("DELETE", "/api/game-profiles/valheim", ""); code != 200 || strings.Contains(body, "(mine)") {
		t.Errorf("delete: %d %s", code, body)
	}
	if code, _ := do("DELETE", "/api/game-profiles/valheim", ""); code != 404 {
		t.Errorf("delete built-in: %d", code)
	}
	if code, _ := do("GET", "/api/game-profiles/nope/yaml", ""); code != 404 {
		t.Errorf("missing yaml: %d", code)
	}
	if code, _ := do("POST", "/api/game-profiles/reload", ""); code != 200 {
		t.Errorf("reload: %d", code)
	}
}
