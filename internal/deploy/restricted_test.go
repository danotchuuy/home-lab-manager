package deploy

import (
	"reflect"
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

func restrictedApp() *catalog.App {
	return &catalog.App{
		ID: "t1", Network: "bridge", ExtraParams: "--memory 1g", PostArgs: "serve",
		Branches: []catalog.Branch{{Tag: "beta"}},
		Config: []catalog.Config{
			{ID: "pw", Name: "Password", Type: catalog.TypeVariable, Target: "PW", Value: "x"},
			{ID: "port", Name: "Port", Type: catalog.TypePort, Target: "25565", Value: "25565"},
			{ID: "data", Name: "Data", Type: catalog.TypePath, Target: "/data", Value: "/srv/data"},
			{ID: "gpu", Name: "GPU", Type: catalog.TypeDevice, Value: "/dev/dri"},
			{ID: "lbl", Name: "Label", Type: catalog.TypeLabel, Target: "a", Value: "b"},
		},
	}
}

func ref[T any](v T) *T { return &v }

// Every field of Request must be classified, so that adding one means
// deciding whether non-admins may change it.
func TestRestrictedChangesClassifiesEveryField(t *testing.T) {
	for _, f := range reflect.VisibleFields(reflect.TypeOf(Request{})) {
		_, locked := lockedFields[f.Name]
		if !locked && !freeFields[f.Name] && f.Name != "Values" {
			t.Errorf("Request.%s isn't in freeFields or lockedFields", f.Name)
		}
	}
}

func TestRestrictedChanges(t *testing.T) {
	old := Request{TemplateID: "t1", Name: "mc", Values: map[string]string{"pw": "old", "data": "/srv/mc"}}
	extra := old
	extra.Extra = []ExtraConfig{{Type: "Path", Target: "/x", Value: "/srv/x"}}

	tests := []struct {
		name string
		base Request
		edit func(*Request)
		want []string
	}{
		{"nothing", old, func(*Request) {}, nil},
		{"variable", old, func(r *Request) { r.Values = map[string]string{"pw": "new", "data": "/srv/mc"} }, nil},
		{"port", old, func(r *Request) { r.Values = map[string]string{"port": "1", "pw": "old", "data": "/srv/mc"} }, nil},
		{"tag and restart policy", old, func(r *Request) { r.Tag, r.RestartPolicy = "beta", "always" }, nil},
		{"template omitted", old, func(r *Request) { r.TemplateID = "" }, nil},
		{"defaults spelled out", old, func(r *Request) {
			r.Network, r.ExtraParams, r.PostArgs, r.Privileged = "bridge", ref("--memory 1g"), ref("serve"), ref(false)
			r.Disabled, r.Extra = []string{}, []ExtraConfig{}
		}, nil},
		{"template default value spelled out", old, func(r *Request) {
			r.Values = map[string]string{"pw": "old", "data": " /srv/mc ", "gpu": "/dev/dri"}
		}, nil},

		{"host path", old, func(r *Request) { r.Values = map[string]string{"pw": "old", "data": "/"} }, []string{"Data"}},
		{"path dropped back to default", old, func(r *Request) { r.Values = map[string]string{"pw": "old"} }, []string{"Data"}},
		{"device", old, func(r *Request) {
			r.Values = map[string]string{"pw": "old", "data": "/srv/mc", "gpu": "/dev/sda"}
		}, []string{"GPU"}},
		{"label", old, func(r *Request) {
			r.Values = map[string]string{"pw": "old", "data": "/srv/mc", "lbl": "c"}
		}, []string{"Label"}},
		{"another template", old, func(r *Request) { r.TemplateID = "t2" }, []string{"TemplateID"}},
		{"host network", old, func(r *Request) { r.Network = "host" }, []string{"Network"}},
		{"address", old, func(r *Request) { r.IPAddress = "10.0.0.2" }, []string{"IPAddress"}},
		{"privileged", old, func(r *Request) { r.Privileged = ref(true) }, []string{"Privileged"}},
		{"extra params", old, func(r *Request) { r.ExtraParams = ref("--cap-add ALL") }, []string{"ExtraParams"}},
		{"post args", old, func(r *Request) { r.PostArgs = ref("sh") }, []string{"PostArgs"}},
		{"disabled", old, func(r *Request) { r.Disabled = []string{"data"} }, []string{"Disabled"}},
		{"extra", old, func(r *Request) {
			r.Extra = []ExtraConfig{{Type: "Path", Target: "/host", Value: "/"}}
		}, []string{"Extra"}},
		// Build reads Values for extra items too, by their generated IDs.
		{"extra item through values", extra, func(r *Request) {
			r.Values = map[string]string{"pw": "old", "data": "/srv/mc", "extra:0": "/"}
		}, []string{"extra:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := tt.base
			tt.edit(&next)
			if got := RestrictedChanges(restrictedApp(), tt.base, next); !slices.Equal(got, tt.want) {
				t.Errorf("restricted = %v, want %v", got, tt.want)
			}
		})
	}
}
