package manager

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/selfexe"
)

// UseExecutable sets hlm's own binary, for helpers that run it in
// containers on other hosts.
func (m *Manager) UseExecutable(path string) { m.executable = path }

// probeBinary is where hlm's binary goes in a probe helper.
const probeBinary = "/hlm-probe"

// StartTestProbe starts a helper next to a copy on another host, on the
// copy's test network: hlm's binary in the copy's own image (already on
// that host), running probeCommand with args, as nobody, without
// capabilities, in 64 MiB. TestProbe reads how it went.
func (m *Manager) StartTestProbe(ctx context.Context, tc *TestCopy, probeCommand string, args []string) error {
	if why := selfexe.Check(m.executable); why != "" {
		return fmt.Errorf("hlm can't ask a copy on another host: %s", why)
	}
	c, err := m.docker.client(tc.Host)
	if err != nil {
		return err
	}
	info, err := m.inspectTestCopy(ctx, tc)
	if err != nil {
		return err
	}
	img, err := c.InspectImage(ctx, info.Image)
	if err != nil {
		return fmt.Errorf("inspect the server's image: %w", err)
	}
	if why := selfexe.Fits(img.Os, img.Architecture); why != "" {
		return fmt.Errorf("hlm can't ask a copy on another host: %s", why)
	}
	_ = c.RemoveContainer(ctx, tc.probeName(), true) // an earlier one
	pids, mem := int64(64), int64(64<<20)
	id, _, err := c.CreateContainer(ctx, tc.probeName(), docker.CreateRequest{
		Image:      info.Image,
		User:       "65534:65534",
		Entrypoint: []string{probeBinary},
		Cmd:        append([]string{probeCommand}, args...),
		Labels:     map[string]string{LabelRestoreTest: tc.Server},
		HostConfig: docker.HostConfig{
			NetworkMode:   tc.Network,
			RestartPolicy: docker.RestartPolicy{Name: "no"},
			CapDrop:       []string{"ALL"},
			SecurityOpt:   []string{"no-new-privileges"},
			Memory:        mem,
			PidsLimit:     &pids,
		},
	})
	if err != nil {
		return fmt.Errorf("create the probe: %w", err)
	}
	if err := selfexe.CopyInto(ctx, c, id, m.executable, probeBinary); err != nil {
		return fmt.Errorf("copy hlm into the probe: %w", err)
	}
	if err := c.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start the probe: %w", err)
	}
	return nil
}

// ErrNoProbe is a copy without a probe helper.
var ErrNoProbe = errors.New("the probe isn't running")

// TestProbe says how a copy's probe helper is doing: done once it has
// exited, with what it printed and its exit code.
func (m *Manager) TestProbe(ctx context.Context, tc *TestCopy) (output string, exitCode int, done bool, err error) {
	c, err := m.docker.client(tc.Host)
	if err != nil {
		return "", 0, false, err
	}
	info, err := c.InspectContainer(ctx, tc.probeName())
	if docker.IsNotFound(err) {
		return "", 0, false, ErrNoProbe
	}
	if err != nil {
		return "", 0, false, err
	}
	if info.Config.Labels[LabelRestoreTest] != tc.Server {
		return "", 0, false, errors.New("that isn't a restore test's probe")
	}
	if info.State.Running {
		return "", 0, false, nil
	}
	r, err := c.Logs(ctx, tc.probeName(), docker.LogsOptions{Tail: "all"})
	if err != nil {
		return "", info.State.ExitCode, true, err
	}
	defer r.Close()
	data, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	return string(data), info.State.ExitCode, true, nil
}
