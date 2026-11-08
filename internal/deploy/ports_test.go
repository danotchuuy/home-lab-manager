package deploy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func rangeApp(target, value string) *catalog.App {
	app := testApp()
	app.Config = []catalog.Config{
		{ID: "port:" + target + "/udp", Name: "Game Ports", Type: catalog.TypePort, Target: target, Mode: "udp", Value: value, Required: true},
		{ID: "port:8080/tcp", Name: "Web", Type: catalog.TypePort, Target: "8080", Mode: "tcp", Value: "8080"},
	}
	return app
}

// bindings returns the host port bound to each container port of proto.
func bindings(c docker.CreateRequest, proto string) map[string]string {
	out := map[string]string{}
	for key, b := range c.HostConfig.PortBindings {
		if strings.HasSuffix(key, "/"+proto) {
			out[strings.TrimSuffix(key, "/"+proto)] = b[0].HostIP + b[0].HostPort
		}
	}
	return out
}

func TestPortRanges(t *testing.T) {
	tests := []struct {
		name, target, value string
		want                map[string]string
	}{
		{"same range", "2456-2458", "2456-2458", map[string]string{"2456": "2456", "2457": "2457", "2458": "2458"}},
		{"shifted range", "2456-2458", "3456-3458", map[string]string{"2456": "3456", "2457": "3457", "2458": "3458"}},
		{"start port only", "2456-2458", "4000", map[string]string{"2456": "4000", "2457": "4001", "2458": "4002"}},
		{"colon range", "49152:49153", "49152:49153", map[string]string{"49152": "49152", "49153": "49153"}},
		{"host ip", "2456-2457", "127.0.0.1:2456-2457", map[string]string{"2456": "127.0.0.12456", "2457": "127.0.0.12457"}},
		{"single with ip", "2456", "0.0.0.0:2456", map[string]string{"2456": "0.0.0.02456"}},
	}
	for _, tt := range tests {
		spec, err := Build(rangeApp(tt.target, tt.value), Request{Name: "s"}, Env{})
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got := bindings(spec.Create, "udp"); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: bindings = %v, want %v", tt.name, got, tt.want)
		}
		for p := range tt.want {
			if _, ok := spec.Create.ExposedPorts[p+"/udp"]; !ok {
				t.Errorf("%s: %s/udp not exposed", tt.name, p)
			}
		}
	}
}

func TestPortRangeErrors(t *testing.T) {
	tests := map[string]struct{ target, value, want string }{
		"size mismatch": {"2456-2458", "3000-3001", "don't match"},
		"past 65535":    {"2456-2458", "65535", "past port 65535"},
		"backwards":     {"2456-2458", "3002-3000", "invalid host port"},
		"not a number":  {"2456-2458", "abc", "invalid host port"},
		"zero":          {"2456", "0", "invalid host port"},
	}
	for name, tt := range tests {
		_, err := Build(rangeApp(tt.target, tt.value), Request{Name: "s"}, Env{})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tt.want)
		}
	}
}

func TestLargePortRangeWarns(t *testing.T) {
	spec, err := Build(rangeApp("50000-51500", "50000-51500"), Request{Name: "s"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Create.HostConfig.PortBindings) != 1502 {
		t.Errorf("got %d bindings", len(spec.Create.HostConfig.PortBindings))
	}
	if len(spec.Warnings) == 0 || !strings.Contains(spec.Warnings[0], "1501 ports") {
		t.Errorf("warnings = %v", spec.Warnings)
	}
}

func TestPortRangeOnMacvlan(t *testing.T) {
	app := rangeApp("2456-2458", "2456-2458")
	spec, err := Build(app, Request{Name: "s", Network: "br0"}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Create.HostConfig.PortBindings != nil || len(spec.Create.ExposedPorts) != 4 {
		t.Errorf("bindings = %v, exposed = %v", spec.Create.HostConfig.PortBindings, spec.Create.ExposedPorts)
	}
}

func TestExtraParamsPublishRange(t *testing.T) {
	c := docker.CreateRequest{Labels: map[string]string{}}
	var warnings []string
	if err := applyExtraParams(&c, "-p 27015-27016:27015-27016/udp -p 127.0.0.1:8080:80 -p 9000", &warnings); err != nil {
		t.Fatal(err)
	}
	if got := bindings(c, "udp"); !reflect.DeepEqual(got, map[string]string{"27015": "27015", "27016": "27016"}) {
		t.Errorf("udp bindings = %v", got)
	}
	if got := bindings(c, "tcp"); !reflect.DeepEqual(got, map[string]string{"80": "127.0.0.18080"}) {
		t.Errorf("tcp bindings = %v", got)
	}
	if _, ok := c.ExposedPorts["9000/tcp"]; !ok {
		t.Error("-p 9000 not exposed")
	}
}

func TestUnraidPorts(t *testing.T) {
	env := []string{"UDP_PORT_15637=15637", "TCP_PORT_8080=8081", "SERVER_PORT=1", "UDP_PORT_99999=1", "udp_port_5=5", "TZ=UTC"}
	got := UnraidPorts(env)
	if strings.Join(got, ",") != "15637/udp,8080/tcp" {
		t.Errorf("UnraidPorts = %v", got)
	}
	all := DeclaredPorts(map[string]struct{}{"27015/udp": {}}, env)
	if len(all) != 3 {
		t.Errorf("DeclaredPorts = %v", all)
	}
	for _, k := range []string{"27015/udp", "15637/udp", "8080/tcp"} {
		if _, ok := all[k]; !ok {
			t.Errorf("no %s in %v", k, all)
		}
	}
}
