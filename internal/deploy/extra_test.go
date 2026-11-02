package deploy

import (
	"slices"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestExtraConfigs(t *testing.T) {
	spec, err := Build(testApp(), Request{
		Name: "s",
		Extra: []ExtraConfig{
			{Type: "Path", Name: "Mods", Target: "/serverdata/mods", Value: "/srv/mods", Mode: "ro"},
			{Type: "Port", Target: "27020", Value: "27020", Mode: "udp"},
			{Type: "Port", Target: "7000-7001", Value: "17000"},
			{Type: "Variable", Target: "EXTRA_FLAG", Value: ""},
			{Type: "Variable", Target: "SERVER_NAME", Value: "Overridden"},
			{Type: "Label", Target: "com.example.team", Value: "blue"},
			{Type: "Device", Value: "/dev/dri"},
		},
	}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Create
	if !slices.Contains(c.HostConfig.Binds, "/srv/mods:/serverdata/mods:ro") {
		t.Errorf("binds = %v", c.HostConfig.Binds)
	}
	if b := c.HostConfig.PortBindings["27020/udp"]; len(b) != 1 || b[0].HostPort != "27020" {
		t.Errorf("udp 27020 = %v", b)
	}
	if b := c.HostConfig.PortBindings["7001/tcp"]; len(b) != 1 || b[0].HostPort != "17001" {
		t.Errorf("tcp 7001 = %v", b)
	}
	if !slices.Contains(c.Env, "EXTRA_FLAG=") {
		t.Errorf("env = %v", c.Env)
	}
	// An extra variable with the same name comes after the template's, so
	// Docker uses it.
	if i, j := slices.Index(c.Env, "SERVER_NAME=Docker"), slices.Index(c.Env, "SERVER_NAME=Overridden"); i < 0 || j < i {
		t.Errorf("override order: env = %v", c.Env)
	}
	if c.Labels["com.example.team"] != "blue" {
		t.Errorf("labels = %v", c.Labels)
	}
	if len(c.HostConfig.Devices) != 1 || c.HostConfig.Devices[0].PathInContainer != "/dev/dri" {
		t.Errorf("devices = %+v", c.HostConfig.Devices)
	}
}

func TestExtraConfigErrors(t *testing.T) {
	tests := map[string]struct {
		extra ExtraConfig
		want  string
	}{
		"relative container path": {ExtraConfig{Type: "Path", Target: "data", Value: "/srv"}, "must be absolute"},
		"relative host path":      {ExtraConfig{Type: "Path", Target: "/data", Value: "srv"}, "must be absolute"},
		"empty host path":         {ExtraConfig{Type: "Path", Target: "/data"}, "is required"},
		"bad path mode":           {ExtraConfig{Type: "Path", Target: "/data", Value: "/srv", Mode: "rwx"}, "access mode"},
		"bad port":                {ExtraConfig{Type: "Port", Target: "http", Value: "80"}, "container port"},
		"bad protocol":            {ExtraConfig{Type: "Port", Target: "80", Value: "80", Mode: "sctp"}, "tcp or udp"},
		"bad host port":           {ExtraConfig{Type: "Port", Target: "80", Value: "eighty"}, "invalid host port"},
		"bad variable":            {ExtraConfig{Type: "Variable", Target: "A=B", Value: "x"}, "not a valid variable"},
		"reserved label":          {ExtraConfig{Type: "Label", Target: "io.hlm.managed", Value: "false"}, "reserved"},
		"reserved legacy label":   {ExtraConfig{Type: "Label", Target: "io.gsh.managed", Value: "false"}, "reserved"},
		"bad device":              {ExtraConfig{Type: "Device", Value: "/etc/passwd"}, "under /dev"},
		"unknown type":            {ExtraConfig{Type: "Volume"}, "unknown type"},
	}
	for name, tt := range tests {
		_, err := Build(testApp(), Request{Name: "s", Extra: []ExtraConfig{tt.extra}}, Env{})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tt.want)
		}
	}
}

func TestTemplateOverrides(t *testing.T) {
	app := testApp()
	app.PostArgs = "+map default"

	// Without overrides the template's values apply.
	spec, err := Build(app, Request{Name: "s"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Create.StopTimeout == nil || *spec.Create.StopTimeout != 30 || !slices.Equal(spec.Create.Cmd, []string{"+map", "default"}) {
		t.Errorf("template defaults: stop=%v cmd=%v", spec.Create.StopTimeout, spec.Create.Cmd)
	}

	spec, err = Build(app, Request{
		Name:        "s",
		ExtraParams: ptr("--memory=4g -e TZ=Europe/Oslo --cpus 2"),
		PostArgs:    ptr(`+map "dust 2"`),
		Privileged:  ptr(true),
	}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Create
	if c.StopTimeout != nil || len(c.HostConfig.Ulimits) != 0 {
		t.Errorf("template ExtraParams still applied: stop=%v ulimits=%v", c.StopTimeout, c.HostConfig.Ulimits)
	}
	if c.HostConfig.Memory != 4<<30 || c.HostConfig.NanoCPUs != 2e9 || !slices.Contains(c.Env, "TZ=Europe/Oslo") {
		t.Errorf("override: memory=%d cpus=%d env=%v", c.HostConfig.Memory, c.HostConfig.NanoCPUs, c.Env)
	}
	if !slices.Equal(c.Cmd, []string{"+map", "dust 2"}) || !c.HostConfig.Privileged {
		t.Errorf("cmd=%q privileged=%v", c.Cmd, c.HostConfig.Privileged)
	}

	// An empty override clears the template's value.
	spec, err = Build(app, Request{Name: "s", ExtraParams: ptr(""), PostArgs: ptr("")}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Create.StopTimeout != nil || spec.Create.Cmd != nil {
		t.Errorf("cleared: stop=%v cmd=%v", spec.Create.StopTimeout, spec.Create.Cmd)
	}
	if _, err := Build(app, Request{Name: "s", PostArgs: ptr(`"unterminated`)}, Env{}); err == nil {
		t.Error("bad post arguments: want error")
	}
}

func TestExtraRoundTrip(t *testing.T) {
	req := Request{
		Name:        "s",
		Extra:       []ExtraConfig{{Type: "Variable", Target: "A", Value: "1"}},
		ExtraParams: ptr(""),
		Privileged:  ptr(false),
	}
	spec, err := Build(testApp(), req, Env{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := RequestFromLabels(spec.Create.Labels)
	if !ok || len(got.Extra) != 1 || got.ExtraParams == nil || *got.ExtraParams != "" || got.Privileged == nil || *got.Privileged {
		t.Errorf("round trip = %+v", got)
	}
	if got.PostArgs != nil {
		t.Error("unset PostArgs should stay unset so template changes still apply")
	}
}

func TestRestartPolicyOverride(t *testing.T) {
	// The template's ExtraParams say --restart=unless-stopped.
	spec, err := Build(testApp(), Request{Name: "s", RestartPolicy: "no"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Create.HostConfig.RestartPolicy.Name; got != "no" {
		t.Errorf("restart policy = %q, want no", got)
	}
	if _, err := Build(testApp(), Request{Name: "s", RestartPolicy: "sometimes"}, Env{}); err == nil {
		t.Error("invalid policy: want error")
	}
}
