package deploy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

const testRoot = "/srv/appdata"

func confinedApp() *catalog.App {
	return &catalog.App{ID: "t", Name: "Game", Repository: "game:latest", Config: []catalog.Config{
		{ID: "data", Name: "Data", Type: catalog.TypePath, Target: "/data", Value: "/srv/appdata/game"},
		{ID: "cfg", Name: "Config", Type: catalog.TypePath, Target: "/data/config", Value: "/srv/appdata/game/config"},
		{ID: "pw", Name: "Password", Type: catalog.TypeVariable, Target: "PW"},
		{ID: "port", Name: "Port", Type: catalog.TypePort, Target: "27015", Value: "27015"},
	}}
}

func confine(t *testing.T, app *catalog.App, req Request) (*Spec, []string) {
	t.Helper()
	spec, err := Build(app, ConfinePaths(app, req, testRoot), Env{})
	if err != nil {
		t.Fatal(err)
	}
	return spec, Confine(spec, testRoot)
}

func TestConfinedServer(t *testing.T) {
	spec, bad := confine(t, confinedApp(), Request{Name: "s1"})
	if len(bad) > 0 {
		t.Fatalf("refused: %v", bad)
	}
	if spec.Create.Labels[LabelConfined] != "true" {
		t.Error("not marked as confined")
	}
	want := []string{"/srv/appdata/s1/data:/data:rw", "/srv/appdata/s1/data-config:/data/config:rw"}
	if !reflect.DeepEqual(spec.Create.HostConfig.Binds, want) {
		t.Errorf("binds = %v, want %v", spec.Create.HostConfig.Binds, want)
	}
}

func TestConfinePathsIgnoresWhatsAsked(t *testing.T) {
	spec, bad := confine(t, confinedApp(), Request{Name: "s1", Values: map[string]string{"data": "/", "cfg": "/var/run/docker.sock"}})
	if len(bad) > 0 {
		t.Fatalf("refused: %v", bad)
	}
	for _, b := range spec.Create.HostConfig.Binds {
		if !strings.HasPrefix(b, "/srv/appdata/s1/") {
			t.Errorf("bind %s", b)
		}
	}
}

func TestConfineRefuses(t *testing.T) {
	yes, str := true, func(s string) *string { return &s }
	for _, try := range []struct {
		name string
		req  Request
		app  func(*catalog.App)
		want string
	}{
		{name: "privileged request", req: Request{Privileged: &yes}, want: "privileged"},
		{name: "privileged template", app: func(a *catalog.App) { a.Privileged = true }, want: "privileged"},
		{name: "privileged flag", req: Request{ExtraParams: str("--privileged")}, want: "privileged"},
		{name: "capability", req: Request{ExtraParams: str("--cap-add=SYS_ADMIN")}, want: "capabilities"},
		{name: "template capability", app: func(a *catalog.App) { a.ExtraParams = "--cap-add NET_ADMIN" }, want: "capabilities"},
		{name: "device flag", req: Request{ExtraParams: str("--device /dev/sda")}, want: "devices"},
		{name: "gpu", req: Request{ExtraParams: str("--gpus all")}, want: "DeviceRequests"},
		{name: "runtime", req: Request{ExtraParams: str("--runtime=sysbox")}, want: "Runtime"},
		{name: "sysctl", req: Request{ExtraParams: str("--sysctl kernel.core_pattern=x")}, want: "kernel"},
		{name: "seccomp", req: Request{ExtraParams: str("--security-opt seccomp=unconfined")}, want: "seccomp"},
		{name: "apparmor", req: Request{ExtraParams: str("--security-opt apparmor=unconfined")}, want: "apparmor"},
		{name: "mount flag", req: Request{ExtraParams: str("-v /:/host")}, want: "folder /"},
		{name: "socket flag", req: Request{ExtraParams: str("-v /var/run/docker.sock:/var/run/docker.sock")}, want: "docker.sock"},
		{name: "traversal flag", req: Request{ExtraParams: str("-v /srv/appdata/s1/../../..:/h:rw")}, want: "folder"},
		{name: "another server's folder", req: Request{ExtraParams: str("-v /srv/appdata/s12:/h:rw")}, want: "s12"},
		{name: "named volume", req: Request{ExtraParams: str("-v shared:/h:rw")}, want: "shared"},
		{name: "nested mount", req: Request{ExtraParams: str("-v /srv/appdata/s1:/h:rw")}, want: "inside"},
		{name: "shared mount", req: Request{ExtraParams: str("-v /srv/appdata/s1/x:/h:rw,shared")}, want: "mount option"},
		{name: "added path", req: Request{Extra: []ExtraConfig{{Type: catalog.TypePath, Name: "x", Target: "/h", Value: "/etc"}}}, want: "folder /etc"},
		{name: "added device", req: Request{Extra: []ExtraConfig{{Type: catalog.TypeDevice, Name: "x", Value: "/dev/sda"}}}, want: "devices"},
		{name: "host network", req: Request{Network: "host"}, want: "host network"},
		{name: "template host network", app: func(a *catalog.App) { a.Network = "host" }, want: "host network"},
		{name: "network flag", req: Request{ExtraParams: str("--network=host")}, want: "host network"},
		{name: "low port", req: Request{Values: map[string]string{"port": "22"}}, want: "port 22"},
		{name: "low port flag", req: Request{ExtraParams: str("-p 80:80")}, want: "port 80"},
		{name: "log driver", req: Request{ExtraParams: str("--log-driver=syslog")}, want: "syslog"},
		{name: "hlm label", req: Request{ExtraParams: str("-l io.hlm.cloudflared=true")}, want: "label"},
		{name: "old hlm label", req: Request{ExtraParams: str("-l io.gsh.wireguard=true")}, want: "label"},
	} {
		t.Run(try.name, func(t *testing.T) {
			app := confinedApp()
			if try.app != nil {
				try.app(app)
			}
			req := try.req
			req.Name = "s1"
			spec, bad := confine(t, app, req)
			if !strings.Contains(strings.Join(bad, "; "), try.want) {
				t.Errorf("got %q, want it to mention %q", bad, try.want)
			}
			if spec.Create.Labels[LabelConfined] != "" {
				t.Error("marked as confined")
			}
		})
	}
}

// Flags Build has no way to apply must be ignored, not passed on.
func TestConfineUnsupportedFlags(t *testing.T) {
	for _, flag := range []string{"--pid=host", "--ipc=host", "--uts=host", "--userns=host", "--cgroupns=host", "--cgroup-parent=/", "--volumes-from=other", "--mount=type=bind,source=/,target=/h"} {
		app := confinedApp()
		app.ExtraParams = flag
		spec, bad := confine(t, app, Request{Name: "s1"})
		if len(bad) > 0 {
			t.Errorf("%s: %v", flag, bad)
		}
		if got := len(spec.Create.HostConfig.Binds); got != 2 {
			t.Errorf("%s: %d binds", flag, got)
		}
	}
}

func TestConfineAllowsHarmlessSettings(t *testing.T) {
	app := confinedApp()
	app.ExtraParams = "--memory=1g --cpus=2 --init --ulimit nofile=1024:2048 --shm-size=64m --tmpfs /tmp --cap-drop=ALL --security-opt=no-new-privileges --restart=on-failure --stop-timeout=30 --user=1000:1000"
	if _, bad := confine(t, app, Request{Name: "s1", Network: "none"}); len(bad) > 0 {
		t.Errorf("refused: %v", bad)
	}
}

// fill gives every kind of field used by the structs something non-zero.
func fill(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Struct:
		for i := range v.NumField() {
			fill(t, v.Field(i))
		}
	default:
		t.Fatalf("no value for a %s", v.Kind())
	}
}

// A Docker setting added later is refused until someone decides it's safe.
func TestConfineRefusesWhatItDoesntKnow(t *testing.T) {
	check := func(v reflect.Value, free map[string]bool, describeOf func(string) string) {
		for _, f := range reflect.VisibleFields(v.Type()) {
			if free[f.Name] {
				continue
			}
			spec := &Spec{Name: "s1", Create: docker.CreateRequest{Labels: map[string]string{}}}
			spec.Create.HostConfig.NetworkMode = "bridge"
			fill(t, reflect.ValueOf(&spec.Create).Elem().FieldByName(describeOf(f.Name)))
			if len(Confine(spec, testRoot)) == 0 {
				t.Errorf("%s isn't refused", f.Name)
			}
		}
	}
	check(reflect.ValueOf(docker.CreateRequest{}), confinedConfig, func(n string) string { return n })

	host := reflect.ValueOf(docker.HostConfig{})
	for _, f := range reflect.VisibleFields(host.Type()) {
		if confinedHost[f.Name] {
			continue
		}
		spec := &Spec{Name: "s1", Create: docker.CreateRequest{Labels: map[string]string{}}}
		spec.Create.HostConfig.NetworkMode = "bridge"
		fill(t, reflect.ValueOf(&spec.Create.HostConfig).Elem().FieldByIndex(f.Index))
		if len(Confine(spec, testRoot)) == 0 {
			t.Errorf("%s isn't refused", f.Name)
		}
	}
}
