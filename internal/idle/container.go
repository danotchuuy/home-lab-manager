package idle

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/selfexe"
)

// LabelListener marks wake listener containers, with the server's name.
const LabelListener = "io.hlm.wake-listener"

// listenerBinary is where hlm's binary goes in a wake listener.
const listenerBinary = "/hlm-wake"

// checkExecutable says why hlm's binary can't run in a wake listener, if it
// can't: it has to be a static Linux binary, since it runs in the game's
// image.
func checkExecutable(path string) string {
	if why := selfexe.Check(path); why != "" {
		return "waking on a connection: " + why
	}
	return ""
}

// listenerPorts are the ports a wake listener takes over: the server's
// published ones, which Docker frees while it's stopped.
func listenerPorts(info docker.ContainerJSON) (map[string][]docker.PortBinding, error) {
	switch mode := info.HostConfig.NetworkMode; {
	case mode == "host":
		return nil, errors.New("waking on a connection needs published ports, and this server uses the host's network")
	case strings.HasPrefix(mode, "container:"):
		return nil, errors.New("waking on a connection needs published ports, and this server uses another container's network")
	}
	out := map[string][]docker.PortBinding{}
	for port, b := range info.HostConfig.PortBindings {
		if len(b) > 0 {
			out[port] = b
		}
	}
	if len(out) == 0 {
		return nil, errors.New("waking on a connection needs published ports, and this server has none (on a macvlan " +
			"network, say); it can still be woken from the status page")
	}
	return out, nil
}

// listenerArgs are a listener's command line for a server.
func (s *Service) listenerArgs(name string, info docker.ContainerJSON, bindings map[string][]docker.PortBinding) []string {
	var tcp, udp []string
	keys := make([]string, 0, len(bindings))
	for k := range bindings {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		port, proto, _ := strings.Cut(k, "/")
		if proto == "udp" {
			udp = append(udp, port)
		} else if proto == "tcp" || proto == "" {
			tcp = append(tcp, port)
		}
	}
	args := []string{ListenerCommand}
	if len(tcp) > 0 {
		args = append(args, "--tcp", strings.Join(tcp, ","))
	}
	if len(udp) > 0 {
		args = append(args, "--udp", strings.Join(udp, ","))
	}
	if p := gameprofile.Match(info.Config.Image); p != nil && p.Query.Port > 0 {
		port := p.Query.Port
		for _, kv := range info.Config.Env {
			if k, v, _ := strings.Cut(kv, "="); k == p.Query.PortEnv && p.Query.PortEnv != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
					port = n
				}
			}
		}
		switch p.Query.Protocol {
		case "minecraft":
			args = append(args, "--minecraft", strconv.Itoa(port))
		case "bedrock":
			args = append(args, "--bedrock", strconv.Itoa(port))
		}
	}
	if _, max, _ := s.opts.Players(name); max > 0 {
		args = append(args, "--max", strconv.Itoa(max))
	}
	return args
}

// listen starts a sleeping server's wake listener, recording it or why it
// couldn't.
func (s *Service) listen(ctx context.Context, name string) {
	id, err := s.startListener(ctx, name)
	m, _ := s.asleepAll()
	a, ok := m[name]
	if !ok {
		if id != "" { // woken meanwhile
			s.removeListener(ctx, name, id)
		}
		return
	}
	a.Listener, a.ListenerError = id, ""
	if err != nil {
		a.ListenerError = err.Error()
		s.opts.Log.Warn("start wake listener", "server", name, "err", err)
	}
	s.setAsleep(name, &a)
	if id != "" {
		s.watch(name, id)
	}
}

func (s *Service) startListener(ctx context.Context, name string) (string, error) {
	if s.listenerProblem != "" {
		return "", errors.New(s.listenerProblem)
	}
	info, err := s.opts.Servers.Inspect(ctx, name)
	if err != nil {
		return "", err
	}
	if info.State.Running {
		return "", errors.New("it's running")
	}
	bindings, err := listenerPorts(info)
	if err != nil {
		return "", err
	}
	host, bare := manager.SplitID(name)
	d, err := s.opts.DockerFor(host)
	if err != nil {
		return "", err
	}
	img, err := d.InspectImage(ctx, info.Image)
	if err != nil {
		return "", fmt.Errorf("inspect the server's image: %w", err)
	}
	if why := selfexe.Fits(img.Os, img.Architecture); why != "" {
		return "", fmt.Errorf("%s, so hlm can't run in it", why)
	}
	// A listener left over from before is in the way.
	if old, err := d.ListContainers(ctx, LabelListener+"="+name); err == nil {
		for _, c := range old {
			d.RemoveContainer(ctx, c.ID, true)
		}
	}
	exposed := map[string]struct{}{}
	for k := range bindings {
		exposed[k] = struct{}{}
	}
	network := info.HostConfig.NetworkMode
	if network == "" || network == "default" {
		network = "bridge"
	}
	pids, mem := int64(64), int64(64<<20)
	hc := docker.HostConfig{
		PortBindings:  bindings,
		NetworkMode:   network,
		RestartPolicy: docker.RestartPolicy{Name: "no"},
		CapDrop:       []string{"ALL"},
		SecurityOpt:   []string{"no-new-privileges"},
		Memory:        mem,
		PidsLimit:     &pids,
	}
	id, _, err := d.CreateContainer(ctx, "hlm-wake-"+bare, docker.CreateRequest{
		Image:        info.Image,
		User:         "65534:65534",
		Entrypoint:   []string{listenerBinary},
		Cmd:          s.listenerArgs(name, info, bindings),
		Labels:       map[string]string{LabelListener: name},
		ExposedPorts: exposed,
		HostConfig:   hc,
	})
	if err != nil {
		return "", fmt.Errorf("create the wake listener: %w", err)
	}
	fail := func(err error) (string, error) {
		d.RemoveContainer(context.Background(), id, true)
		return "", err
	}
	if err := copyBinary(ctx, d, id, s.opts.Executable); err != nil {
		return fail(fmt.Errorf("copy hlm into the wake listener: %w", err))
	}
	if err := d.StartContainer(ctx, id); err != nil {
		return fail(fmt.Errorf("start the wake listener: %w", err))
	}
	return id, nil
}

// copyBinary puts hlm's binary into a created container.
func copyBinary(ctx context.Context, d Docker, id, path string) error {
	return selfexe.CopyInto(ctx, d, id, path, listenerBinary)
}

func (s *Service) docker(name string) (Docker, error) {
	host, _ := manager.SplitID(name)
	return s.opts.DockerFor(host)
}

func (s *Service) listenerRunning(ctx context.Context, name, id string) bool {
	d, err := s.docker(name)
	if err != nil {
		return false
	}
	info, err := d.InspectContainer(ctx, id)
	return err == nil && info.State.Running
}

func (s *Service) removeListener(ctx context.Context, name, id string) {
	s.mu.Lock()
	if s.watching[name] == id {
		delete(s.watching, name)
	}
	s.mu.Unlock()
	d, err := s.docker(name)
	if err != nil {
		return
	}
	if err := d.RemoveContainer(ctx, id, true); err != nil && !docker.IsNotFound(err) {
		s.opts.Log.Warn("remove wake listener", "server", name, "err", err)
	}
}

// watch waits for a listener to exit: having seen a player, it wakes the
// server; having stopped otherwise, it's started again later.
func (s *Service) watch(name, id string) {
	s.mu.Lock()
	if s.watching[name] == id {
		s.mu.Unlock()
		return
	}
	s.watching[name] = id
	ctx := s.runCtx
	s.mu.Unlock()
	go func() {
		d, err := s.docker(name)
		if err != nil {
			return
		}
		code, err := d.WaitContainer(ctx, id)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		current := s.watching[name] == id
		if current {
			delete(s.watching, name)
		}
		s.mu.Unlock()
		if !current {
			return // removed by hlm
		}
		if err == nil && code == 0 {
			// It only exits cleanly when someone came to play.
			why := "a connection"
			if c, ok := lastContact(ctx, d, id); ok {
				why = describe(c)
			}
			if err := s.Wake(ctx, name, why); err != nil && !errors.Is(err, ErrNotAsleep) {
				s.opts.Log.Warn("wake server", "server", name, "err", err)
			}
			return
		}
		why := fmt.Sprintf("the wake listener stopped (exit status %d)", code)
		if err != nil {
			why = "the wake listener stopped: " + err.Error()
		}
		s.opts.Log.Warn("wake listener stopped", "server", name, "why", why)
		m, _ := s.asleepAll()
		if a, ok := m[name]; ok && a.Listener == id {
			a.Listener, a.ListenerError = "", why+"; trying again in 5 minutes"
			s.setAsleep(name, &a)
			d.RemoveContainer(ctx, id, true)
			if s.opts.Sleep(ctx, retryListener) == nil {
				if m, _ := s.asleepAll(); m[name].Listener == "" {
					if _, ok := m[name]; ok {
						s.listen(ctx, name)
					}
				}
			}
		}
	}()
}

// lastContact reads what woke a listener from its output.
func lastContact(ctx context.Context, d Docker, id string) (Contact, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := d.Logs(ctx, id, docker.LogsOptions{Tail: "5"})
	if err != nil {
		return Contact{}, false
	}
	defer r.Close()
	var c Contact
	ok := false
	sc := bufio.NewScanner(io.LimitReader(r, 64<<10))
	for sc.Scan() {
		var got Contact
		if json.Unmarshal(sc.Bytes(), &got) == nil && got.Port != "" {
			c, ok = got, true
		}
	}
	return c, ok
}
