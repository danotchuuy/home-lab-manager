package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const tricky = `user www-data;
# stream { this is a comment }
events { worker_connections 768; }
http {
    log_format x '{ stream { }';
    upstream stream { server 127.0.0.1:8080; }
}
stream {
	upstream db { server 10.0.0.2:5432; }
	server {
		listen 5432;
		proxy_pass db; # stream { }
	}
}
`

func TestStreamBlock(t *testing.T) {
	b, ok := streamBlock(tricky)
	if !ok || b.included || !strings.HasPrefix(tricky[b.start:], "stream {\n\tupstream db") || tricky[b.close] != '}' || b.close != len(tricky)-2 {
		t.Fatalf("block = %+v, %v", b, ok)
	}
	if got := blockText(tricky, b); !strings.HasPrefix(got, "stream {") || !strings.HasSuffix(got, "\t}\n}") {
		t.Errorf("block text:\n%s", got)
	}
	updated := insertInclude(tricky, b.open)
	if !strings.Contains(updated, "stream {\n\t# Added by Home Lab Manager: relays for game servers.\n\tinclude /etc/nginx/hlm-stream.conf;\n\tupstream db") {
		t.Errorf("updated:\n%s", updated)
	}
	if nb, ok := streamBlock(updated); !ok || !nb.included {
		t.Error("the updated block doesn't include hlm's file")
	}
	// Only the stream block changed.
	if strings.Replace(updated, "\n\t# Added by Home Lab Manager: relays for game servers.\n\tinclude /etc/nginx/hlm-stream.conf;", "", 1) != tricky {
		t.Error("other parts of the file changed")
	}
	for name, src := range map[string]string{
		"none":        "http { upstream stream { } }\n",
		"comment":     "# stream {\n}\n",
		"unbalanced":  "stream {\n server {\n",
		"as argument": "events { } map $a stream { }\n",
	} {
		if _, ok := streamBlock(src); ok {
			t.Errorf("%s: found a stream block", name)
		}
	}
	if b, _ := streamBlock("stream{include \"/etc/nginx/hlm-stream.conf\";}"); !b.included {
		t.Error("quoted include not recognized")
	}
	if b, _ := streamBlock("stream{include /etc/nginx/gsh-stream.conf;}"); !b.included {
		t.Error("include of the file from before the rename not recognized")
	}
}

func TestParseDump(t *testing.T) {
	dump := "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok\n" +
		"# configuration file /etc/nginx/nginx.conf:\nevents {}\ninclude /etc/nginx/streams.conf;\n\n" +
		"# configuration file /etc/nginx/streams.conf:\nstream {\n}\n\n"
	files := parseDump(dump)
	if len(files) != 2 || files[1].path != "/etc/nginx/streams.conf" || !strings.HasPrefix(files[1].content, "stream {") {
		t.Fatalf("files = %+v", files)
	}
}

// fakeHost simulates the relay's files for the adopt scripts.
type fakeHost struct {
	files     map[string]string
	rejectAll bool // nginx -t fails after the change
}

func (h *fakeHost) dump() string {
	out := "nginx: configuration file /etc/nginx/nginx.conf test is successful\n"
	for _, p := range []string{"/etc/nginx/nginx.conf", "/etc/nginx/streams.conf"} {
		if c, ok := h.files[p]; ok {
			out += "# configuration file " + p + ":\n" + c + "\n"
		}
	}
	return out
}

func (h *fakeHost) run(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
	switch {
	case script == dumpScript:
		return remote.Result{Stdout: h.dump()}, nil
	case strings.HasPrefix(script, "cat -- "):
		p := strings.Trim(strings.TrimPrefix(script, "cat -- "), "'")
		c, ok := h.files[p]
		if !ok {
			return remote.Result{Stderr: "No such file", ExitCode: 1}, nil
		}
		return remote.Result{Stdout: c}, nil
	case strings.Contains(script, "hlm:adopted"):
		lines := strings.SplitN(script, "\n", 3)
		p := strings.Trim(strings.TrimPrefix(lines[0], "f="), "'")
		want := strings.TrimPrefix(lines[1], "want=")
		sum := sha256.Sum256([]byte(h.files[p]))
		if hex.EncodeToString(sum[:]) != want {
			return remote.Result{Stdout: "hlm:changed\n", ExitCode: 3}, nil
		}
		if h.rejectAll {
			return remote.Result{Stdout: "nginx: [emerg] duplicate listen\nhlm:testfailed\n", ExitCode: 4}, nil
		}
		h.files[p] = string(stdin)
		return remote.Result{Stdout: "nginx: test is successful\nhlm:adopted\n"}, nil
	}
	return remote.Result{}, errors.New("unexpected script")
}

func TestAdopt(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}})
	host := &fakeHost{files: map[string]string{
		"/etc/nginx/nginx.conf":   "events {}\ninclude /etc/nginx/streams.conf;\n",
		"/etc/nginx/streams.conf": "stream {\n    server { listen 25; proxy_pass mail:25; }\n}\n",
	}}
	s.run = host.run
	ctx := context.Background()

	a, err := s.PlanAdoption(ctx)
	if err != nil || !a.Found || a.Included || a.File != "/etc/nginx/streams.conf" || !strings.Contains(a.Proposed, "    include /etc/nginx/hlm-stream.conf;") {
		t.Fatalf("plan = %+v, %v", a, err)
	}

	// nginx rejects it: an error, and the file is unchanged.
	host.rejectAll = true
	if _, _, err := s.Adopt(ctx); err == nil || !strings.Contains(err.Error(), "put back") {
		t.Errorf("rejected: %v", err)
	}
	host.rejectAll = false

	a, _, err = s.Adopt(ctx)
	if err != nil || !a.Included || !strings.Contains(host.files["/etc/nginx/streams.conf"], includeLine) {
		t.Fatalf("adopt = %+v, %v\n%s", a, err, host.files["/etc/nginx/streams.conf"])
	}
	// Adopting again changes nothing.
	before := host.files["/etc/nginx/streams.conf"]
	if a, _, err := s.Adopt(ctx); err != nil || !a.Included || host.files["/etc/nginx/streams.conf"] != before {
		t.Errorf("second adopt: %+v, %v", a, err)
	}

	// No stream block: nothing to adopt.
	host.files = map[string]string{"/etc/nginx/nginx.conf": "events {}\n"}
	if a, err := s.PlanAdoption(ctx); err != nil || a.Found {
		t.Errorf("no block: %+v, %v", a, err)
	}
}
