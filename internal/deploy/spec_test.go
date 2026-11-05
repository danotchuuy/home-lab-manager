package deploy

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func testApp() *catalog.App {
	return &catalog.App{
		ID:          "enshrouded-test",
		Name:        "Enshrouded",
		Repository:  "nodiaque/steamcmd:enshrouded",
		Network:     "bridge",
		ExtraParams: "--restart=unless-stopped --stop-timeout=30 --ulimit nofile=10240:10240",
		WebUI:       "http://[IP]:[PORT:8080]/",
		Branches:    []catalog.Branch{{Tag: "beta"}, {Tag: "proton", Repository: "other/image:proton"}},
		Config: []catalog.Config{
			{ID: "port:15637/udp", Name: "Query", Type: catalog.TypePort, Target: "15637", Mode: "udp", Value: "15637", Required: true},
			{ID: "port:8080/tcp", Name: "Web", Type: catalog.TypePort, Target: "8080", Mode: "tcp", Value: "8080"},
			{ID: "path:/serverdata", Name: "Files", Type: catalog.TypePath, Target: "/serverdata", Mode: "rw", Value: "/srv/appdata/enshrouded", Required: true},
			{ID: "variable:SERVER_NAME", Name: "Name", Type: catalog.TypeVariable, Target: "SERVER_NAME", Value: "Docker"},
			{ID: "variable:PASSWORD", Name: "Password", Type: catalog.TypeVariable, Target: "PASSWORD", Value: ""},
		},
	}
}

func TestBuild(t *testing.T) {
	spec, err := Build(testApp(), Request{
		TemplateID: "enshrouded-test",
		Name:       "my-server",
		Values: map[string]string{
			"port:15637/udp":       "25637",
			"variable:SERVER_NAME": "Hello",
		},
	}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Create
	if c.Image != "nodiaque/steamcmd:enshrouded" {
		t.Errorf("image = %q", c.Image)
	}
	if got := c.HostConfig.PortBindings["15637/udp"]; len(got) != 1 || got[0].HostPort != "25637" {
		t.Errorf("udp binding = %+v", got)
	}
	if !slices.Contains(c.HostConfig.Binds, "/srv/appdata/enshrouded:/serverdata:rw") {
		t.Errorf("binds = %v", c.HostConfig.Binds)
	}
	if !slices.Contains(c.Env, "SERVER_NAME=Hello") || !slices.Contains(c.Env, "PASSWORD=") {
		t.Errorf("env = %v", c.Env)
	}
	if c.HostConfig.RestartPolicy.Name != "unless-stopped" || c.StopTimeout == nil || *c.StopTimeout != 30 {
		t.Errorf("restart/stop timeout = %+v / %v", c.HostConfig.RestartPolicy, c.StopTimeout)
	}
	if want := []docker.Ulimit{{Name: "nofile", Soft: 10240, Hard: 10240}}; !reflect.DeepEqual(c.HostConfig.Ulimits, want) {
		t.Errorf("ulimits = %+v", c.HostConfig.Ulimits)
	}
	if c.Labels[LabelManaged] != "true" || c.Labels[LabelTemplate] != "enshrouded-test" {
		t.Errorf("labels = %v", c.Labels)
	}
	if c.Labels[LabelWebUI] != "http://[IP]:8080/" {
		t.Errorf("webui = %q", c.Labels[LabelWebUI])
	}
	req, ok := RequestFromLabels(c.Labels)
	if !ok || req.Name != "my-server" || req.Values["variable:SERVER_NAME"] != "Hello" {
		t.Errorf("request label round trip = %+v, %v", req, ok)
	}
}

func TestBuildBranches(t *testing.T) {
	for tag, want := range map[string]string{
		"beta":   "nodiaque/steamcmd:beta",
		"proton": "other/image:proton",
	} {
		spec, err := Build(testApp(), Request{Name: "s", Tag: tag}, Env{})
		if err != nil {
			t.Fatal(err)
		}
		if spec.Image != want {
			t.Errorf("tag %s: image = %q, want %q", tag, spec.Image, want)
		}
	}
	if _, err := Build(testApp(), Request{Name: "s", Tag: "nope"}, Env{}); err == nil {
		t.Error("unknown tag: want error")
	}
}

func TestBuildValidation(t *testing.T) {
	tests := map[string]Request{
		"bad name":            {Name: "bad name"},
		"required port empty": {Name: "s", Values: map[string]string{"port:15637/udp": ""}},
		"bad port":            {Name: "s", Values: map[string]string{"port:15637/udp": "99999"}},
		"relative path":       {Name: "s", Values: map[string]string{"path:/serverdata": "data"}},
		"disable required":    {Name: "s", Disabled: []string{"path:/serverdata"}},
	}
	for name, req := range tests {
		if _, err := Build(testApp(), req, Env{}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	// Optional items can be disabled.
	spec, err := Build(testApp(), Request{Name: "s", Disabled: []string{"port:8080/tcp"}}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Create.HostConfig.PortBindings["8080/tcp"]; ok {
		t.Error("disabled port was published")
	}
}

func TestHostNetwork(t *testing.T) {
	app := testApp()
	app.Network = "host"
	app.ExtraParams = "--hostname=gameserver"
	spec, err := Build(app, Request{Name: "s"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	hc := spec.Create.HostConfig
	if hc.NetworkMode != "host" || len(hc.PortBindings) != 0 || spec.Create.Hostname != "" {
		t.Errorf("host network: mode=%q bindings=%v hostname=%q", hc.NetworkMode, hc.PortBindings, spec.Create.Hostname)
	}
}

func TestExtraParams(t *testing.T) {
	var c docker.CreateRequest
	c.Labels = map[string]string{}
	var warnings []string
	err := applyExtraParams(&c, `--memory=8g --cpus 2.5 --shm-size=512m --cap-add=SYS_NICE --gpus all --init -e "A=b c" --sysctl net.core.somaxconn=1024 --mystery-flag=1`, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	hc := c.HostConfig
	if hc.Memory != 8<<30 || hc.NanoCPUs != 2_500_000_000 || hc.ShmSize != 512<<20 {
		t.Errorf("memory/cpus/shm = %d/%d/%d", hc.Memory, hc.NanoCPUs, hc.ShmSize)
	}
	if !slices.Contains(hc.CapAdd, "SYS_NICE") || len(hc.DeviceRequests) != 1 || hc.DeviceRequests[0].Count != -1 {
		t.Errorf("cap-add/gpus = %v/%+v", hc.CapAdd, hc.DeviceRequests)
	}
	if hc.Init == nil || !*hc.Init || !slices.Contains(c.Env, "A=b c") || hc.Sysctls["net.core.somaxconn"] != "1024" {
		t.Errorf("init/env/sysctl = %v/%v/%v", hc.Init, c.Env, hc.Sysctls)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want one for --mystery-flag", warnings)
	}
}

func TestHealthParams(t *testing.T) {
	var c docker.CreateRequest
	c.Labels = map[string]string{}
	var warnings []string
	err := applyExtraParams(&c, `--health-cmd "curl -f http://localhost:8080/ || exit 1" --health-interval=30s --health-timeout 5s --health-start-period=10m --health-retries 4`, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	want := docker.Healthcheck{Test: []string{"CMD-SHELL", "curl -f http://localhost:8080/ || exit 1"},
		Interval: int64(30 * time.Second), Timeout: int64(5 * time.Second), StartPeriod: int64(10 * time.Minute), Retries: 4}
	if c.Healthcheck == nil || !reflect.DeepEqual(*c.Healthcheck, want) || len(warnings) != 0 {
		t.Errorf("healthcheck = %+v, warnings %v", c.Healthcheck, warnings)
	}

	// Only timings keep the image's command.
	c = docker.CreateRequest{Labels: map[string]string{}}
	if err := applyExtraParams(&c, "--health-start-period 15m", &warnings); err != nil {
		t.Fatal(err)
	}
	if c.Healthcheck == nil || c.Healthcheck.Test != nil || c.Healthcheck.StartPeriod != int64(15*time.Minute) {
		t.Errorf("timings only = %+v", c.Healthcheck)
	}

	c = docker.CreateRequest{Labels: map[string]string{}}
	if err := applyExtraParams(&c, "--no-healthcheck", &warnings); err != nil {
		t.Fatal(err)
	}
	if c.Healthcheck == nil || !slices.Equal(c.Healthcheck.Test, []string{"NONE"}) {
		t.Errorf("--no-healthcheck = %+v", c.Healthcheck)
	}

	for _, bad := range []string{"--health-interval 30", "--health-retries x", "--health-timeout=-5s"} {
		c = docker.CreateRequest{Labels: map[string]string{}}
		if err := applyExtraParams(&c, bad, &warnings); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	tests := map[string][]string{
		`a b  c`:                     {"a", "b", "c"},
		`+set fs_homepath "/home/x"`: {"+set", "fs_homepath", "/home/x"},
		`-c 'echo "hi"; exit 1'`:     {"-c", `echo "hi"; exit 1`},
		`a\ b ""`:                    {"a b", ""},
		``:                           nil,
	}
	for in, want := range tests {
		got, err := splitArgs(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitArgs(`"open`); err == nil {
		t.Error("unterminated quote: want error")
	}
}

func TestImageDefaultsAreNotPinned(t *testing.T) {
	app := catalog.FromImage(catalog.ImageInfo{
		Ref: "example/game:1",
		Env: []string{"MAX_PLAYERS=8", "JAVA_VERSION=21", "MOTD=hello"},
	})
	spec, err := Build(app, Request{Name: "s", Values: map[string]string{"variable:MOTD": "Welcome!"}}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	// Only the variable the user changed is set; the image supplies the rest,
	// so a newer image can change its own defaults.
	if strings.Join(spec.Create.Env, ",") != "MOTD=Welcome!" {
		t.Errorf("env = %v", spec.Create.Env)
	}
}

func TestChangedSettings(t *testing.T) {
	params := "--memory=4g"
	old := Request{Values: map[string]string{"port": "2456", "pw": "a", "name": "x"}, Network: "bridge"}
	new := Request{Values: map[string]string{"port": "2457", "pw": "b", "world": "y", "name": "x"}, Network: "br0", ExtraParams: &params}
	got := ChangedSettings(old, new)
	want := []string{"port", "pw", "world", "network", "extraParams"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("changed = %q, want %q", got, want)
	}
	if got := ChangedSettings(old, old); len(got) != 0 {
		t.Errorf("unchanged = %q", got)
	}
}
