package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
)

// LabelRestoreTest marks the throwaway copies (and their networks and
// volumes) that restore tests make, with the server they test.
const LabelRestoreTest = "io.hlm.restore-test"

// maxTestVolumes bounds the data folders a copy gets, and so the volume
// names cleaned up after it.
const maxTestVolumes = 32

// TestCopy is a throwaway copy of a server, for testing that a backup
// restores and starts: the same image (by ID) and settings, fresh volumes
// in place of the server's data folders, no published ports, and its own
// internal network, which reaches neither the internet nor the LAN.
type TestCopy struct {
	Server  string
	Name    string
	ID      string
	Network string
	Volumes []string
	// joined says hlm's own container was connected to the network, to
	// reach the copy.
	joined bool
}

// testNames are the names a server's test copy uses.
func testNames(server string) (container, network string, volume func(int) string) {
	_, bare := SplitID(server)
	base := "hlm-restore-test-" + bare
	return base, base, func(i int) string { return fmt.Sprintf("%s-%d", base, i) }
}

// ErrTestRemote is a restore test asked for on another Docker host.
var ErrTestRemote = errors.New("restore tests run only for servers on hlm's own Docker host for now")

// CreateTestCopy makes a test copy of a server (stopped, with empty data
// folders). Leftovers of an earlier test of it are removed first.
func (m *Manager) CreateTestCopy(ctx context.Context, server string) (*TestCopy, error) {
	info, err := m.inspect(ctx, server)
	if err != nil {
		return nil, err
	}
	if host, _ := SplitID(info.ID); host != LocalHost {
		return nil, ErrTestRemote
	}
	if info.HostConfig.NetworkMode == "host" {
		return nil, errors.New("the server uses the host's network, so a copy would clash with it; restore tests need a server on a bridge network")
	}
	name := strings.TrimPrefix(info.Name, "/")
	if err := m.RemoveTestCopies(ctx, name); err != nil {
		return nil, fmt.Errorf("remove an earlier test's copy: %w", err)
	}
	cname, nname, vname := testNames(name)
	labels := map[string]string{LabelRestoreTest: name}
	tc := &TestCopy{Server: name, Name: cname, Network: nname}
	fail := func(err error) (*TestCopy, error) {
		m.RemoveTestCopy(context.WithoutCancel(ctx), tc)
		return nil, err
	}

	if _, err := m.docker.local.CreateNetwork(ctx, docker.CreateNetworkRequest{
		Name: nname, Driver: "bridge", Internal: true, Labels: labels,
	}); err != nil {
		return fail(fmt.Errorf("create the test network: %w", err))
	}

	// Fresh volumes where the server has data folders: the copy never
	// sees the server's own data.
	var binds []string
	for _, mnt := range info.Mounts {
		if mnt.Type != "bind" && mnt.Type != "volume" {
			continue
		}
		if len(tc.Volumes) == maxTestVolumes {
			return fail(fmt.Errorf("the server has more than %d data folders", maxTestVolumes))
		}
		v := vname(len(tc.Volumes))
		if err := m.docker.local.CreateVolume(ctx, v, labels); err != nil {
			return fail(fmt.Errorf("create a test volume: %w", err))
		}
		tc.Volumes = append(tc.Volumes, v)
		binds = append(binds, v+":"+mnt.Destination)
	}

	raw, err := m.docker.local.InspectContainerRaw(ctx, info.ID)
	if err != nil {
		return fail(err)
	}
	body, err := testCopyBody(raw, labels, nname, binds)
	if err != nil {
		return fail(err)
	}
	if tc.ID, err = m.docker.local.CreateContainerRaw(ctx, cname, body); err != nil {
		return fail(fmt.Errorf("create the test copy: %w", err))
	}
	m.log.Info("made a restore test copy", "server", name, "copy", cname)
	return tc, nil
}

// copiedHostConfig are the settings a copy keeps from the server's
// HostConfig: its limits and how it runs, not its ports, devices,
// privileges, mounts or networks.
var copiedHostConfig = []string{
	"Memory", "MemoryReservation", "MemorySwap", "NanoCpus", "CpuShares", "CpuQuota", "CpuPeriod", "CpusetCpus",
	"PidsLimit", "ShmSize", "Init", "Tmpfs", "Ulimits", "Sysctls", "Runtime",
}

// testCopyBody turns a server's inspect output into a copy's create body.
func testCopyBody(raw map[string]any, labels map[string]string, network string, binds []string) (map[string]any, error) {
	cfg, ok := raw["Config"].(map[string]any)
	if !ok {
		return nil, errors.New("the server's settings couldn't be read")
	}
	image, _ := raw["Image"].(string) // the image's ID: the one it runs now
	if image == "" {
		return nil, errors.New("the server's image couldn't be read")
	}
	body := map[string]any{}
	for k, v := range cfg {
		switch k {
		case "Hostname", "Domainname", "Labels", "Image", "MacAddress":
		default:
			body[k] = v
		}
	}
	body["Image"] = image
	body["Labels"] = labels
	host := map[string]any{}
	if hc, ok := raw["HostConfig"].(map[string]any); ok {
		for _, k := range copiedHostConfig {
			if v, ok := hc[k]; ok && v != nil {
				host[k] = v
			}
		}
	}
	host["NetworkMode"] = network
	host["RestartPolicy"] = map[string]any{"Name": "no"}
	host["Binds"] = binds
	host["PortBindings"] = map[string]any{}
	host["PublishAllPorts"] = false
	body["HostConfig"] = host
	return body, nil
}

// inspectTestCopy inspects a test copy, checking it is one.
func (m *Manager) inspectTestCopy(ctx context.Context, tc *TestCopy) (docker.ContainerJSON, error) {
	info, err := m.docker.local.InspectContainer(ctx, tc.ID)
	if err != nil {
		return info, err
	}
	if info.Config.Labels[LabelRestoreTest] != tc.Server {
		return info, errors.New("that isn't a restore test copy")
	}
	return info, nil
}

// InspectTestCopy is the copy as Docker sees it.
func (m *Manager) InspectTestCopy(ctx context.Context, tc *TestCopy) (docker.ContainerJSON, error) {
	return m.inspectTestCopy(ctx, tc)
}

// RestoreTestCopy extracts a backup (a tar stream, as ArchiveFolders makes)
// into a stopped test copy.
func (m *Manager) RestoreTestCopy(ctx context.Context, tc *TestCopy, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	info, err := m.inspectTestCopy(ctx, tc)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	// Docker's archive API only: the volumes are the copy's own.
	return files.New(m.docker.local, nil).Restore(ctx, info, r, progress)
}

// StartTestCopy starts a copy, and connects hlm's own container (when it
// runs in one) to its network so hlm can reach it.
func (m *Manager) StartTestCopy(ctx context.Context, tc *TestCopy) error {
	if _, err := m.inspectTestCopy(ctx, tc); err != nil {
		return err
	}
	if m.selfID != "" && !tc.joined {
		if err := m.docker.local.ConnectNetwork(ctx, tc.Network, m.selfID, nil); err != nil {
			return fmt.Errorf("connect hlm to the test network: %w", err)
		}
		tc.joined = true
	}
	return m.docker.local.StartContainer(ctx, tc.ID)
}

// TestCopyLogs is the end of a copy's log.
func (m *Manager) TestCopyLogs(ctx context.Context, tc *TestCopy, lines int) string {
	r, err := m.docker.local.Logs(ctx, tc.ID, docker.LogsOptions{Tail: fmt.Sprint(lines)})
	if err != nil {
		return ""
	}
	defer r.Close()
	data, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	return string(data)
}

// RemoveTestCopy removes a copy, its volumes and its network.
func (m *Manager) RemoveTestCopy(ctx context.Context, tc *TestCopy) error {
	var errs []error
	if tc.ID != "" {
		if err := m.docker.local.RemoveContainer(ctx, tc.ID, true); err != nil && !docker.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if tc.joined {
		// It may not be connected (a leftover from before it was); removing
		// the network below says if it still is.
		_ = m.docker.local.DisconnectNetwork(ctx, tc.Network, m.selfID, true)
		tc.joined = false
	}
	for _, v := range tc.Volumes {
		if err := m.docker.local.RemoveVolume(ctx, v); err != nil && !docker.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if err := m.docker.local.RemoveNetwork(ctx, tc.Network); err != nil && !docker.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RemoveTestCopies removes whatever a server's earlier tests left behind
// (hlm stopped mid-test, say); with server "", every server's.
func (m *Manager) RemoveTestCopies(ctx context.Context, server string) error {
	filter := LabelRestoreTest
	if server != "" {
		filter += "=" + server
	}
	list, err := m.docker.local.ListContainers(ctx, filter)
	if err != nil {
		return err
	}
	servers := map[string]bool{}
	if server != "" {
		servers[server] = true
	}
	for _, c := range list {
		servers[c.Labels[LabelRestoreTest]] = true
	}
	if server == "" {
		// Networks without a container left, too.
		if nets, err := m.docker.local.ListNetworks(ctx); err == nil {
			for _, n := range nets {
				if s := n.Labels[LabelRestoreTest]; s != "" {
					servers[s] = true
				}
			}
		}
	}
	var errs []error
	for s := range servers {
		cname, nname, vname := testNames(s)
		tc := &TestCopy{Server: s, Name: cname, Network: nname}
		for _, c := range list {
			if c.Labels[LabelRestoreTest] == s {
				tc.ID = c.ID
			}
		}
		if tc.ID == "" {
			tc.ID = cname
		}
		if m.selfID != "" {
			tc.joined = true
		}
		for i := range maxTestVolumes {
			tc.Volumes = append(tc.Volumes, vname(i))
		}
		if err := m.RemoveTestCopy(ctx, tc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
