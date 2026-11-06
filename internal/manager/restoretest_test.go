package manager

import (
	"encoding/json"
	"slices"
	"testing"
)

// The copy keeps how the server runs and its limits, and nothing that
// reaches outside: no ports, devices, privileges, mounts or networks.
func TestTestCopyBody(t *testing.T) {
	var raw map[string]any
	json.Unmarshal([]byte(`{
		"Image": "sha256:abc",
		"Config": {"Image": "itzg/minecraft-server", "Hostname": "mc", "Env": ["EULA=TRUE"], "Cmd": ["run"],
			"User": "1000", "Labels": {"io.hlm.managed": "true"}, "ExposedPorts": {"25565/tcp": {}}},
		"HostConfig": {"Memory": 2147483648, "NanoCpus": 2000000000, "Privileged": true, "CapAdd": ["NET_ADMIN"],
			"PortBindings": {"25565/tcp": [{"HostPort": "25565"}]}, "Binds": ["/srv/mc:/data"], "NetworkMode": "bridge",
			"Devices": [{"PathOnHost": "/dev/dri"}], "ExtraHosts": ["x:1.2.3.4"], "RestartPolicy": {"Name": "unless-stopped"}}
	}`), &raw)
	body, err := testCopyBody(raw, map[string]string{LabelRestoreTest: "mc"}, "hlm-restore-test-mc", []string{"hlm-restore-test-mc-0:/data"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	var got struct {
		Image      string
		Hostname   string
		User       string
		Env        []string
		Labels     map[string]string
		HostConfig map[string]any
	}
	json.Unmarshal(b, &got)
	if got.Image != "sha256:abc" || got.Hostname != "" || got.User != "1000" || !slices.Equal(got.Env, []string{"EULA=TRUE"}) {
		t.Errorf("config: %s", b)
	}
	if len(got.Labels) != 1 || got.Labels[LabelRestoreTest] != "mc" {
		t.Errorf("labels: %v", got.Labels)
	}
	hc := got.HostConfig
	for _, k := range []string{"Privileged", "CapAdd", "Devices", "ExtraHosts"} {
		if _, ok := hc[k]; ok {
			t.Errorf("kept %s", k)
		}
	}
	if hc["NetworkMode"] != "hlm-restore-test-mc" || hc["Memory"] != float64(2147483648) || hc["NanoCpus"] != float64(2000000000) {
		t.Errorf("host config: %v", hc)
	}
	if pb, _ := hc["PortBindings"].(map[string]any); len(pb) != 0 {
		t.Errorf("ports: %v", hc["PortBindings"])
	}
	if binds, _ := hc["Binds"].([]any); len(binds) != 1 || binds[0] != "hlm-restore-test-mc-0:/data" {
		t.Errorf("binds: %v", hc["Binds"])
	}
	if rp, _ := hc["RestartPolicy"].(map[string]any); rp["Name"] != "no" {
		t.Errorf("restart: %v", hc["RestartPolicy"])
	}
}
