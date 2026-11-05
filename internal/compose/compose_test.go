package compose

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, file, env string) *Project {
	t.Helper()
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(data, env, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	p := load(t, "nextcloud.yml", "DB_PASSWORD='hunter2' # the db\nexport TZ=\n")
	var order []string
	for _, s := range p.Services {
		order = append(order, s.Name)
	}
	// Dependencies first: cache and db before app, app before cron.
	if !slices.Equal(order, []string{"cache", "db", "app", "cron"}) {
		t.Errorf("order = %v", order)
	}
	app := p.Service("app")
	// The merge gives restart; app's own environment replaces the merged
	// one (YAML merges aren't deep, as in Compose).
	if app.Restart != "unless-stopped" || app.Environment["TZ"] != "" || app.Environment["POSTGRES_PASSWORD"] != "hunter2" ||
		app.MemLimit != 1<<30 || app.CPUs != 1.5 || len(app.Ports) != 2 || app.Ports[1].HostIP != "127.0.0.1" {
		t.Errorf("app = %+v", app)
	}
	if d := app.DependsOn; len(d) != 2 {
		t.Errorf("depends_on = %+v", d)
	}
	db := p.Service("db")
	if db.Healthcheck == nil || db.Healthcheck.Interval != 10*time.Second || db.Healthcheck.Retries != 5 ||
		!slices.Equal(db.Aliases["back"], []string{"postgres"}) {
		t.Errorf("db = %+v %+v", db, db.Healthcheck)
	}
	cache := p.Service("cache")
	if !slices.Equal(cache.Command, []string{"redis-server", "--save", "", "--appendonly", "no"}) || cache.Ulimits[0].Hard != 4096 {
		t.Errorf("cache = %+v", cache)
	}
	if cron := p.Service("cron"); cron.NetworkMode != "service:app" || *cron.StopGrace != 90 || len(cron.Networks) != 0 {
		t.Errorf("cron = %+v", cron)
	}
	if !p.Networks["back"].Internal || p.VolumeName("db") != "shared-pg-data" || p.VolumeName("data") != "cloud_data" ||
		p.NetworkName("default") != "cloud_default" {
		t.Errorf("networks %+v volumes %+v", p.Networks, p.Volumes)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v", p.Warnings)
	}
	// Without the required password: refused.
	data, _ := os.ReadFile("testdata/nextcloud.yml")
	if _, err := Parse(data, "", "cloud"); err == nil || !strings.Contains(err.Error(), "set DB_PASSWORD") {
		t.Errorf("missing required variable: %v", err)
	}
}

func TestSpec(t *testing.T) {
	p := load(t, "nextcloud.yml", "DB_PASSWORD=x")
	spec, err := p.Spec(p.Service("app"), "/srv/appdata")
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Create
	if spec.Name != "cloud-app" || c.Labels[LabelStack] != "cloud" || c.Labels[LabelService] != "app" || c.Labels[labelManaged] != "true" ||
		c.Labels[LabelHash] != spec.Hash || spec.Hash == "" {
		t.Errorf("labels = %v", c.Labels)
	}
	if b := c.HostConfig.PortBindings["80/tcp"]; len(b) != 1 || b[0].HostPort != "8080" {
		t.Errorf("ports = %+v", c.HostConfig.PortBindings)
	}
	if b := c.HostConfig.PortBindings["9000/tcp"]; b[0].HostIP != "127.0.0.1" {
		t.Errorf("ip port = %+v", b)
	}
	if !slices.Equal(c.HostConfig.Binds, []string{"/srv/appdata/cloud/html:/var/www/html", "cloud_data:/var/www/html/data", "/etc/localtime:/etc/localtime:ro"}) {
		t.Errorf("binds = %v", c.HostConfig.Binds)
	}
	if c.HostConfig.NetworkMode != "cloud_default" || !slices.Equal(c.NetworkingConfig.EndpointsConfig["cloud_default"].Aliases, []string{"app"}) ||
		len(spec.Connect) != 1 || spec.Connect[0].Network != "cloud_back" {
		t.Errorf("networks = %s %+v %+v", c.HostConfig.NetworkMode, c.NetworkingConfig, spec.Connect)
	}
	if c.HostConfig.Memory != 1<<30 || c.HostConfig.NanoCPUs != 1_500_000_000 || c.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("host config = %+v", c.HostConfig)
	}
	cron, _ := p.Spec(p.Service("cron"), "/srv/appdata")
	if cron.Create.HostConfig.NetworkMode != "container:cloud-app" || *cron.Create.StopTimeout != 90 || cron.Create.Entrypoint[0] != "/cron.sh" {
		t.Errorf("cron = %+v", cron.Create)
	}
	// The same settings give the same hash; a change gives another.
	again, _ := p.Spec(p.Service("app"), "/srv/appdata")
	p.Service("app").Environment["NEW"] = "1"
	changed, _ := p.Spec(p.Service("app"), "/srv/appdata")
	if again.Hash != spec.Hash || changed.Hash == spec.Hash {
		t.Errorf("hashes %s %s %s", spec.Hash, again.Hash, changed.Hash)
	}
}

func TestRejected(t *testing.T) {
	for _, tt := range []struct{ yaml, want string }{
		{"services:\n  a:\n    build: .\n", "build isn't supported"},
		{"services:\n  a:\n    image: x\n    profiles: [dev]\n", "profiles"},
		{"services:\n  a:\n    image: x\n    depends_on: [b]\n", "isn't in the file"},
		{"services:\n  a:\n    image: x\n    depends_on: [b]\n  b:\n    image: y\n    depends_on: [a]\n", "loop"},
		{"services:\n  a:\n    image: x\n    volumes: ['../../etc:/etc']\n", ""}, // caught by Spec
		{"services:\n  a:\n    image: x\n    env_file: other.env\n", "only the .env"},
		{"services:\n  a:\n    image: x\n    deploy: {replicas: 3}\n", "replicas"},
		{"secrets:\n  a: {}\nservices:\n  a:\n    image: x\n", "secrets"},
		{"services:\n  a:\n    image: x\n    frobnicate: 1\n", "frobnicate isn't supported"},
		{"services:\n  a: {}\n", "no image"},
		{"services:\n  a:\n    image: x\n    ports: ['1:2:3:4']\n", "can't read the port"},
		{"[1, 2]", "a map"},
	} {
		p, err := Parse([]byte(tt.yaml), "", "s")
		if tt.want == "" {
			if err != nil {
				t.Errorf("%q: %v", tt.yaml, err)
				continue
			}
			if _, err := p.Spec(p.Services[0], "/srv/appdata"); err == nil || !strings.Contains(err.Error(), "leaves the stack") {
				t.Errorf("escaping path: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.yaml, err, tt.want)
		}
	}
	if _, err := Parse([]byte("services:\n  a:\n    image: x\n"), "", "Bad Name"); err == nil {
		t.Error("accepted a bad stack name")
	}
	// Missing variables are empty, with a warning; $$ is a dollar.
	p, err := Parse([]byte("services:\n  a:\n    image: x\n    command: echo $$HOME ${NOPE}\n"), "", "s")
	if err != nil || len(p.Warnings) != 1 || !slices.Equal(p.Services[0].Command, []string{"echo", "$HOME"}) {
		t.Errorf("command = %q, warnings %v, %v", p.Services[0].Command, p.Warnings, err)
	}
}
