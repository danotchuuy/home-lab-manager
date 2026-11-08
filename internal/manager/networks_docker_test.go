package manager

import (
	"context"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
)

// A host's interfaces are read through Docker, from a helper on the host's
// network: here, this machine's, so they match its own routing table. (It
// only reads /proc/net/route, with no extra privileges.) Enable with
// HLM_DOCKER_TESTS=1.
func TestDockerHostInterfacesOn(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	// No host /proc collector, as when hlm can't see the host's: the helper
	// reads it.
	m := New(d, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	got, err := m.HostInterfacesOn(ctx, LocalHost)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	want, err := hostmetrics.ParseRoutes(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("through Docker %+v, this machine's %+v", got, want)
	}
	if _, err := m.HostInterfacesOn(ctx, "nope"); err == nil {
		t.Error("an unknown host")
	}
	// The helper is gone.
	list, _ := d.ListContainers(ctx, "io.hlm.cleanup")
	if len(list) != 0 {
		t.Errorf("helpers left: %d", len(list))
	}
}

// A stopped container's fixed address counts as used, so it isn't offered
// or given to another server; the network's automatic range is reported.
func TestDockerStoppedFixedIP(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PullImage(ctx, "busybox:1.37", nil); err != nil {
		t.Fatal(err)
	}
	m := New(d, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	netName, name := "hlm-test-net-"+suffix, "hlm-test-ip-"+suffix
	if _, err := m.CreateNetwork(ctx, LocalHost, NetworkRequest{Name: netName, Driver: "bridge", Subnet: "172.31.251.0/24", IPRange: "172.31.251.224/27"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.RemoveNetwork(context.Background(), LocalHost, netName) })
	ep := docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "172.31.251.230"}}
	id, _, err := d.CreateContainer(ctx, name, docker.CreateRequest{
		Image: "busybox:1.37", Cmd: []string{"true"},
		HostConfig:       docker.HostConfig{NetworkMode: netName},
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{netName: ep}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveContainer(context.Background(), id, true) })

	nets, err := m.Networks(ctx, LocalHost)
	if err != nil {
		t.Fatal(err)
	}
	var got *NetworkInfo
	for i := range nets {
		if nets[i].Name == netName {
			got = &nets[i]
		}
	}
	if got == nil || len(got.IPRanges) != 1 || got.IPRanges[0] != "172.31.251.224/27" {
		t.Fatalf("network %+v", got)
	}
	if len(got.UsedIPs) != 1 || got.UsedIPs[0] != (NetworkIP{IP: "172.31.251.230", Container: name, Stopped: true}) {
		t.Errorf("used %+v", got.UsedIPs)
	}

	spec := &deploy.Spec{Create: docker.CreateRequest{NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{netName: ep}}}}
	if err := m.checkStoppedIP(ctx, LocalHost, spec, ""); err == nil || !strings.Contains(err.Error(), name+" (stopped)") {
		t.Errorf("another server on its address: %v", err)
	}
	// Recreating the container itself keeps its address.
	if err := m.checkStoppedIP(ctx, LocalHost, spec, id); err != nil {
		t.Errorf("its own address: %v", err)
	}
	other := docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "172.31.251.231"}}
	spec.Create.NetworkingConfig.EndpointsConfig[netName] = other
	if err := m.checkStoppedIP(ctx, LocalHost, spec, ""); err != nil {
		t.Errorf("a free address: %v", err)
	}
}
