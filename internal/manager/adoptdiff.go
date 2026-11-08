package manager

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// fromContainer makes a template, and the request for it, that describe a
// container as it is: its published ports, folders, variables, labels and
// devices as the template's items, and the rest as docker run flags
// (ExtraParams) and arguments (PostArgs). Every item's value is in the
// request too, so localizing the template doesn't move its folders.
func fromContainer(host, name string, info docker.ContainerJSON, cur docker.CreateRequest, img docker.Image, direct bool) (*catalog.App, deploy.Request) {
	app := &catalog.App{
		ID: adoptedID("container", host, name), Name: name, Repository: cur.Image,
		Icon: cur.Labels["net.unraid.docker.icon"], WebUI: cur.Labels["net.unraid.docker.webui"],
		Network: cur.HostConfig.NetworkMode, Categories: []string{catalog.CustomCategory}, Config: []catalog.Config{},
		Overview: "Made from the settings of the container " + JoinID(host, name) + " when hlm adopted it.",
	}
	req := deploy.Request{TemplateID: app.ID, Name: name, Values: map[string]string{}}
	ids := map[string]int{}
	add := func(c catalog.Config) {
		ids[c.ID]++
		if n := ids[c.ID]; n > 1 {
			c.ID += "#" + strconv.Itoa(n)
		}
		if c.Name == "" {
			c.Name = c.Target
		}
		c.Default, c.Display = c.Value, "always"
		app.Config = append(app.Config, c)
		req.Values[c.ID] = c.Value
	}

	seen := map[string]bool{}
	for _, k := range slices.Sorted(maps.Keys(cur.HostConfig.PortBindings)) {
		port, proto, _ := strings.Cut(k, "/")
		if proto == "" {
			proto = "tcp"
		}
		for _, b := range cur.HostConfig.PortBindings[k] {
			if b.HostPort == "" {
				continue // a port Docker picks; compareContainers reports it
			}
			v := b.HostPort
			if ip := anyIP(b.HostIP); ip != "" {
				v = ip + ":" + v
			}
			if seen[k+v] {
				continue
			}
			seen[k+v] = true
			add(catalog.Config{ID: "port:" + port + "/" + proto, Name: strings.ToUpper(proto) + " port " + port, Type: catalog.TypePort, Target: port, Value: v, Mode: proto})
		}
	}

	// On the host's network or its own LAN address it's reached on the
	// ports it declares: those it exposes beyond its image's, and those
	// Unraid passes it as variables (UDP_PORT_15637=15637). They become its
	// ports, so they're declared again, and port forwarding and status
	// queries know them. (On a bridge network, a port would be published.)
	if direct {
		unraid := map[string]string{} // port → the value Unraid passed
		for _, kv := range cur.Env {
			if ps := deploy.UnraidPorts([]string{kv}); len(ps) == 1 {
				_, v, _ := strings.Cut(kv, "=")
				unraid[ps[0]] = strings.TrimSpace(v)
			}
		}
		for _, key := range sortedKeys(deploy.DeclaredPorts(cur.ExposedPorts, cur.Env)) {
			if _, fromImage := img.Config.ExposedPorts[key]; fromImage {
				continue // the image declares it itself
			}
			port, proto, _ := strings.Cut(key, "/")
			if proto == "" {
				proto = "tcp"
			}
			v := port
			if u, err := strconv.Atoi(unraid[key]); err == nil && u > 0 && u < 65536 {
				v = strconv.Itoa(u)
			}
			if seen[key+v] {
				continue
			}
			seen[key+v] = true
			add(catalog.Config{ID: "port:" + port + "/" + proto, Name: strings.ToUpper(proto) + " port " + port, Type: catalog.TypePort, Target: port, Value: v, Mode: proto})
		}
	}

	var flags []string
	mounts := slices.Clone(info.Mounts)
	slices.SortFunc(mounts, func(a, b docker.MountPoint) int { return strings.Compare(a.Destination, b.Destination) })
	for _, mt := range mounts {
		mode := "rw"
		if !mt.RW {
			mode = "ro"
		}
		switch mt.Type {
		case "bind":
			add(catalog.Config{ID: "path:" + mt.Destination, Name: "Folder " + mt.Destination, Type: catalog.TypePath, Target: mt.Destination, Value: mt.Source, Mode: mode})
		case "volume":
			// Named and anonymous volumes alike are kept by name, with their
			// data.
			v := mt.Name + ":" + mt.Destination
			if mode == "ro" {
				v += ":ro"
			}
			flags = append(flags, "--volume="+v)
		}
	}

	imgEnv := map[string]bool{}
	for _, kv := range img.Config.Env {
		imgEnv[kv] = true
	}
	for _, kv := range cur.Env {
		if imgEnv[kv] {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		add(catalog.Config{ID: "variable:" + k, Type: catalog.TypeVariable, Target: k, Value: v, Mask: secretName(k)})
	}
	for _, k := range slices.Sorted(maps.Keys(cur.Labels)) {
		if iv, ok := img.Config.Labels[k]; (ok && iv == cur.Labels[k]) || ownLabel(k) {
			continue
		}
		add(catalog.Config{ID: "label:" + k, Type: catalog.TypeLabel, Target: k, Value: cur.Labels[k]})
	}
	for _, d := range cur.HostConfig.Devices {
		add(catalog.Config{ID: "device:" + d.PathInContainer, Name: "Device " + d.PathOnHost, Type: catalog.TypeDevice, Target: d.PathInContainer, Value: d.PathOnHost})
	}
	priv := cur.HostConfig.Privileged
	req.Privileged = &priv

	hc := cur.HostConfig
	each := func(flag string, vals []string) {
		for _, v := range vals {
			flags = append(flags, flag+"="+v)
		}
	}
	each("--cap-add", hc.CapAdd)
	each("--cap-drop", hc.CapDrop)
	each("--security-opt", hc.SecurityOpt)
	for _, k := range slices.Sorted(maps.Keys(hc.Sysctls)) {
		flags = append(flags, "--sysctl="+k+"="+hc.Sysctls[k])
	}
	for _, k := range slices.Sorted(maps.Keys(hc.Tmpfs)) {
		v := k
		if hc.Tmpfs[k] != "" {
			v += ":" + hc.Tmpfs[k]
		}
		flags = append(flags, "--tmpfs="+v)
	}
	each("--add-host", hc.ExtraHosts)
	each("--dns", hc.DNS)
	each("--group-add", hc.GroupAdd)
	for _, u := range hc.Ulimits {
		flags = append(flags, fmt.Sprintf("--ulimit=%s=%d:%d", u.Name, u.Soft, u.Hard))
	}
	if hc.Memory > 0 {
		flags = append(flags, "--memory="+strconv.FormatInt(hc.Memory, 10))
	}
	if hc.NanoCPUs > 0 {
		flags = append(flags, "--cpus="+strconv.FormatFloat(float64(hc.NanoCPUs)/1e9, 'f', -1, 64))
	}
	if hc.CpusetCpus != "" {
		flags = append(flags, "--cpuset-cpus="+hc.CpusetCpus)
	}
	if hc.ShmSize > 0 && hc.ShmSize != defaultShm {
		flags = append(flags, "--shm-size="+strconv.FormatInt(hc.ShmSize, 10))
	}
	if hc.PidsLimit != nil && *hc.PidsLimit > 0 {
		flags = append(flags, "--pids-limit="+strconv.FormatInt(*hc.PidsLimit, 10))
	}
	if hc.Init != nil && *hc.Init {
		flags = append(flags, "--init")
	}
	if hc.ReadonlyRootfs {
		flags = append(flags, "--read-only")
	}
	if hc.Runtime != "" && hc.Runtime != "runc" {
		flags = append(flags, "--runtime="+hc.Runtime)
	}
	if lc := hc.LogConfig; lc != nil && lc.Type != "" && (lc.Type != "json-file" || len(lc.Config) > 0) {
		flags = append(flags, "--log-driver="+lc.Type)
		for _, k := range slices.Sorted(maps.Keys(lc.Config)) {
			flags = append(flags, "--log-opt="+k+"="+lc.Config[k])
		}
	}
	for _, d := range hc.DeviceRequests {
		if !isGPU(d) {
			continue
		}
		switch {
		case len(d.DeviceIDs) > 0:
			flags = append(flags, "--gpus=device="+strings.Join(d.DeviceIDs, ","))
		case d.Count < 0:
			flags = append(flags, "--gpus=all")
		default:
			flags = append(flags, "--gpus="+strconv.Itoa(d.Count))
		}
	}
	if cur.User != "" && cur.User != img.Config.User {
		flags = append(flags, "--user="+cur.User)
	}
	if customHostname(cur, info) {
		flags = append(flags, "--hostname="+cur.Hostname)
	}
	if cur.Tty {
		flags = append(flags, "--tty")
	}
	if cur.StopTimeout != nil {
		flags = append(flags, "--stop-timeout="+strconv.Itoa(*cur.StopTimeout))
	}
	if h := cur.Healthcheck; h != nil && !sameHealth(h, img.Config.Healthcheck) {
		switch {
		case len(h.Test) == 1 && h.Test[0] == "NONE":
			flags = append(flags, "--no-healthcheck")
		case len(h.Test) > 1 && (h.Test[0] == "CMD-SHELL" || h.Test[0] == "CMD"):
			cmd := h.Test[1]
			if h.Test[0] == "CMD" {
				cmd = joinArgs(h.Test[1:])
			}
			flags = append(flags, "--health-cmd="+cmd)
			for flag, ns := range map[string]int64{"--health-interval": h.Interval, "--health-timeout": h.Timeout, "--health-start-period": h.StartPeriod} {
				if ns > 0 {
					flags = append(flags, flag+"="+time.Duration(ns).String())
				}
			}
			if h.Retries > 0 {
				flags = append(flags, "--health-retries="+strconv.Itoa(h.Retries))
			}
		}
	}

	// Its command: docker run --entrypoint drops the image's arguments too,
	// so they go with it.
	postArgs := ""
	switch {
	case !slices.Equal(cur.Entrypoint, img.Config.Entrypoint):
		args := cur.Cmd
		if len(cur.Entrypoint) > 0 {
			flags = append(flags, "--entrypoint="+cur.Entrypoint[0])
			args = append(slices.Clone(cur.Entrypoint[1:]), cur.Cmd...)
		} else {
			flags = append(flags, "--entrypoint=")
		}
		postArgs = joinArgs(args)
	case !slices.Equal(cur.Cmd, img.Config.Cmd):
		postArgs = joinArgs(cur.Cmd)
	}
	extra := joinArgs(flags)
	req.ExtraParams, req.PostArgs = &extra, &postArgs
	return app, req
}

// defaultShm is Docker's /dev/shm size.
const defaultShm = 64 << 20

// anyIP is a port binding's address, "" for all of them.
func anyIP(ip string) string {
	if ip == "0.0.0.0" || ip == "::" {
		return ""
	}
	return ip
}

func secretName(k string) bool {
	l := strings.ToLower(k)
	return strings.Contains(l, "password") || strings.Contains(l, "secret") || strings.Contains(l, "token") ||
		strings.HasSuffix(l, "_key") || strings.Contains(l, "apikey") || strings.Contains(l, "api_key")
}

// ownLabel is a label hlm, Unraid or Compose sets, not the user.
func ownLabel(k string) bool {
	for _, p := range []string{"io.hlm.", "io.gsh.", "net.unraid.docker.", "com.docker.compose."} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func isGPU(d docker.DeviceRequest) bool {
	if d.Driver == "nvidia" {
		return true
	}
	for _, caps := range d.Capabilities {
		if slices.Contains(caps, "gpu") {
			return true
		}
	}
	return false
}

// customHostname says whether the container's host name was set, rather
// than Docker's default (the start of its ID).
func customHostname(cur docker.CreateRequest, info docker.ContainerJSON) bool {
	_, id := SplitID(info.ID)
	return cur.Hostname != "" && !strings.HasPrefix(id, cur.Hostname) && cur.HostConfig.NetworkMode != "host"
}

func sameHealth(a, b *docker.Healthcheck) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.Equal(a.Test, b.Test) && a.Interval == b.Interval && a.Timeout == b.Timeout && a.StartPeriod == b.StartPeriod && a.Retries == b.Retries
}

// joinArgs writes arguments the way deploy reads them back (as a shell
// would), quoting what needs it.
func joinArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = quoteArg(a)
	}
	return strings.Join(out, " ")
}

func quoteArg(a string) string {
	if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:=,@%+-") == "" {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// compareContainers says what a container made from want would do
// differently from the container cur (info, extras) is, in words.
func compareContainers(cur, want docker.CreateRequest, img docker.Image, info docker.ContainerJSON, extras containerExtras, direct bool) []string {
	var out []string
	say := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }

	if catalog.NormalizeImage(cur.Image) != catalog.NormalizeImage(want.Image) {
		say("Its image would be %s, not %s.", want.Image, cur.Image)
	}

	// Published ports.
	ports := func(c docker.CreateRequest) map[string]bool {
		set := map[string]bool{}
		for k, bs := range c.HostConfig.PortBindings {
			for _, b := range bs {
				hp := b.HostPort
				if hp == "" {
					hp = "a port Docker picks"
				}
				if ip := anyIP(b.HostIP); ip != "" {
					hp = ip + ":" + hp
				}
				set[k+" on "+hp] = true
			}
		}
		return set
	}
	if cur.HostConfig.NetworkMode != "host" || want.HostConfig.NetworkMode != "host" {
		cp, wp := ports(cur), ports(want)
		for _, p := range sortedKeys(cp) {
			if !wp[p] {
				say("It would stop publishing port %s.", p)
			}
		}
		for _, p := range sortedKeys(wp) {
			if !cp[p] {
				say("It would publish port %s.", p)
			}
		}
	}
	// On the host's network or its own LAN address, the ports it declares
	// are what port forwarding and status queries go by.
	if direct {
		have := deploy.DeclaredPorts(want.ExposedPorts, want.Env)
		for k := range img.Config.ExposedPorts {
			have[k] = struct{}{}
		}
		for _, k := range sortedKeys(deploy.DeclaredPorts(cur.ExposedPorts, cur.Env)) {
			if _, ok := have[k]; !ok {
				say("It would no longer declare port %s, so port forwarding and status queries wouldn't know about it.", k)
			}
		}
	}
	if extras.HostConfig.PublishAllPorts {
		say("It publishes all its ports on ports Docker picks (-P); hlm only publishes the ports listed.")
	}

	// Folders and volumes, by where they are in the container.
	curM := map[string]string{}
	for _, mt := range info.Mounts {
		src := mt.Source
		if mt.Type == "volume" {
			src = "volume " + shortVolume(mt.Name)
		}
		if !mt.RW {
			src += " (read-only)"
		}
		curM[mt.Destination] = src
	}
	for p, o := range cur.HostConfig.Tmpfs {
		curM[p] = "tmpfs " + o
	}
	wantM := map[string]string{}
	for _, b := range want.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		src, dst := parts[0], parts[1]
		if !strings.HasPrefix(src, "/") {
			src = "volume " + shortVolume(src)
		}
		if len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro") {
			src += " (read-only)"
		}
		wantM[dst] = src
	}
	for p, o := range want.HostConfig.Tmpfs {
		wantM[p] = "tmpfs " + o
	}
	for _, d := range sortedKeys(curM) {
		switch w, ok := wantM[d]; {
		case !ok && strings.HasPrefix(curM[d], "volume ") && strings.HasSuffix(strings.TrimSuffix(curM[d], " (read-only)"), "(anonymous)"):
			say("%s would get a new, empty volume: what's in %s now would no longer be used.", d, curM[d])
		case !ok:
			say("%s would no longer have %s.", d, curM[d])
		case w != curM[d]:
			say("%s would be %s, not %s.", d, w, curM[d])
		}
	}
	for _, d := range sortedKeys(wantM) {
		if _, ok := curM[d]; !ok {
			say("%s would be %s.", d, wantM[d])
		}
	}

	// Variables: the image's, with the container's own on top.
	env := func(lists ...[]string) map[string]string {
		m := map[string]string{}
		for _, l := range lists {
			for _, kv := range l {
				k, v, _ := strings.Cut(kv, "=")
				m[k] = v
			}
		}
		return m
	}
	ce, we := env(cur.Env), env(img.Config.Env, want.Env)
	for _, k := range sortedKeys(ce) {
		switch w, ok := we[k]; {
		case !ok:
			say("The variable %s would no longer be set.", k)
		case w != ce[k]:
			say("The variable %s would have another value.", k)
		}
	}
	for _, k := range sortedKeys(we) {
		if _, ok := ce[k]; !ok {
			say("The variable %s would be set.", k)
		}
	}

	// Labels other than hlm's, Unraid's and Compose's.
	wl := maps.Clone(img.Config.Labels)
	if wl == nil {
		wl = map[string]string{}
	}
	maps.Copy(wl, want.Labels)
	for _, k := range sortedKeys(cur.Labels) {
		if ownLabel(k) {
			continue
		}
		if w, ok := wl[k]; !ok {
			say("It would lose the label %s.", k)
		} else if w != cur.Labels[k] {
			say("The label %s would have another value.", k)
		}
	}

	// Networks.
	curNet, wantNet := netName(cur.HostConfig.NetworkMode), netName(want.HostConfig.NetworkMode)
	if curNet != wantNet {
		say("It would be on the network %s, not %s.", wantNet, curNet)
	}
	for _, n := range sortedKeys(info.NetworkSettings.Networks) {
		if n != curNet && n != cur.HostConfig.NetworkMode {
			say("It would no longer be on the network %s too: hlm's servers are on one network.", n)
		}
	}
	if ep, ok := info.NetworkSettings.Networks[curNet]; ok && curNet == wantNet {
		var we docker.EndpointSettings
		if want.NetworkingConfig != nil {
			we = want.NetworkingConfig.EndpointsConfig[wantNet]
		}
		fixed := func(e docker.EndpointSettings) string {
			if e.IPAMConfig != nil {
				return e.IPAMConfig.IPv4Address
			}
			return ""
		}
		switch cf, wf := fixed(ep), fixed(we); {
		case cf != "" && wf == "":
			say("Its fixed address %s would no longer be fixed.", cf)
		case wf != "" && wf != cf && wf != ep.IPAddress:
			say("Its address would be %s, not %s.", wf, firstNonEmpty(cf, ep.IPAddress, "one Docker picks"))
		}
		if we.MacAddress != "" && ep.MacAddress != "" && !strings.EqualFold(we.MacAddress, ep.MacAddress) {
			say("Its MAC address on %s would be %s, not %s, so your router sees a new device (hlm gives each server its own stable one).", wantNet, we.MacAddress, ep.MacAddress)
		}
	}

	ch, wh := cur.HostConfig, want.HostConfig
	if ch.Privileged != wh.Privileged {
		if ch.Privileged {
			say("It would no longer be privileged.")
		} else {
			say("It would be privileged.")
		}
	}
	devices := func(h docker.HostConfig) []string {
		var l []string
		for _, d := range h.Devices {
			l = append(l, d.PathOnHost+":"+d.PathInContainer)
		}
		return l
	}
	setDiff := func(what string, a, b []string) {
		for _, x := range a {
			if !slices.Contains(b, x) {
				say("It would no longer have the %s %s.", what, x)
			}
		}
		for _, x := range b {
			if !slices.Contains(a, x) {
				say("It would have the %s %s.", what, x)
			}
		}
	}
	setDiff("device", devices(ch), devices(wh))
	setDiff("added capability", ch.CapAdd, wh.CapAdd)
	setDiff("dropped capability", ch.CapDrop, wh.CapDrop)
	setDiff("security option", ch.SecurityOpt, wh.SecurityOpt)
	setDiff("extra host", ch.ExtraHosts, wh.ExtraHosts)
	setDiff("DNS server", ch.DNS, wh.DNS)
	setDiff("extra group", ch.GroupAdd, wh.GroupAdd)
	kv := func(m map[string]string) []string {
		var l []string
		for _, k := range sortedKeys(m) {
			l = append(l, k+"="+m[k])
		}
		return l
	}
	setDiff("sysctl", kv(ch.Sysctls), kv(wh.Sysctls))
	ulimits := func(h docker.HostConfig) []string {
		var l []string
		for _, u := range h.Ulimits {
			l = append(l, fmt.Sprintf("%s=%d:%d", u.Name, u.Soft, u.Hard))
		}
		return l
	}
	setDiff("limit", ulimits(ch), ulimits(wh))
	differ := func(what string, a, b any) {
		if a != b {
			say("Its %s would be %v, not %v.", what, b, a)
		}
	}
	differ("memory limit (bytes)", ch.Memory, wh.Memory)
	differ("CPU limit (nano-CPUs)", ch.NanoCPUs, wh.NanoCPUs)
	differ("CPU pinning", ch.CpusetCpus, wh.CpusetCpus)
	shm := func(n int64) int64 {
		if n == 0 {
			return defaultShm
		}
		return n
	}
	differ("/dev/shm size", shm(ch.ShmSize), shm(wh.ShmSize))
	pids := func(p *int64) int64 {
		if p == nil || *p < 0 {
			return 0
		}
		return *p
	}
	differ("process limit", pids(ch.PidsLimit), pids(wh.PidsLimit))
	init := func(b *bool) bool { return b != nil && *b }
	differ("init process (--init)", init(ch.Init), init(wh.Init))
	differ("read-only root (--read-only)", ch.ReadonlyRootfs, wh.ReadonlyRootfs)
	runtime := func(r string) string { return firstNonEmpty(r, "runc") }
	differ("runtime", runtime(ch.Runtime), runtime(wh.Runtime))
	if ch.LogConfig != nil && wh.LogConfig != nil && ch.LogConfig.Type != wh.LogConfig.Type {
		differ("log driver", ch.LogConfig.Type, wh.LogConfig.Type)
	}
	gpus := func(h docker.HostConfig) int {
		n := 0
		for _, d := range h.DeviceRequests {
			if isGPU(d) {
				n++
			}
		}
		return n
	}
	if gpus(ch) != gpus(wh) {
		say("Its GPU access would change.")
	}
	differ("user", firstNonEmpty(cur.User, img.Config.User, "the image's"), firstNonEmpty(want.User, img.Config.User, "the image's"))
	if customHostname(cur, info) && want.Hostname != cur.Hostname {
		say("Its host name would be %s, not %s.", firstNonEmpty(want.Hostname, "Docker's default"), cur.Hostname)
	}
	differ("terminal (--tty)", cur.Tty, want.Tty)

	// Its command, as the image's with the container's overrides.
	curCmd := strings.TrimSpace(joinArgs(append(slices.Clone(cur.Entrypoint), cur.Cmd...)))
	we2, wc := img.Config.Entrypoint, img.Config.Cmd
	if want.Entrypoint != nil {
		we2, wc = want.Entrypoint, nil
		if len(we2) == 1 && we2[0] == "" {
			we2 = nil
		}
	}
	if want.Cmd != nil {
		wc = want.Cmd
	}
	if wantCmd := strings.TrimSpace(joinArgs(append(slices.Clone(we2), wc...))); wantCmd != curCmd {
		say("It would run %q, not %q.", wantCmd, curCmd)
	}
	if cur.WorkingDir != "" && cur.WorkingDir != img.Config.WorkingDir {
		say("It would start in the image's folder %q, not %q.", firstNonEmpty(img.Config.WorkingDir, "/"), cur.WorkingDir)
	}
	if cur.StopSignal != "" && cur.StopSignal != img.Config.StopSignal && want.StopSignal != cur.StopSignal {
		say("It would be stopped with %s, not %s.", firstNonEmpty(img.Config.StopSignal, "SIGTERM"), cur.StopSignal)
	}
	if rp := info.HostConfig.RestartPolicy; rp.Name == "on-failure" && rp.MaximumRetryCount > 0 {
		say("Its restart policy would keep restarting it on failure, without the limit of %d tries.", rp.MaximumRetryCount)
	}

	// What hlm's servers can't have at all.
	x := extras.HostConfig
	if x.PidMode != "" {
		say("It shares a process namespace (--pid=%s), which hlm doesn't keep.", x.PidMode)
	}
	if x.IpcMode != "" && x.IpcMode != "private" && x.IpcMode != "shareable" {
		say("It shares IPC (--ipc=%s), which hlm doesn't keep.", x.IpcMode)
	}
	if x.UTSMode != "" {
		say("It shares a UTS namespace (--uts=%s), which hlm doesn't keep.", x.UTSMode)
	}
	if len(x.Links) > 0 {
		say("Its links to other containers (--link) wouldn't be kept.")
	}
	if len(x.VolumesFrom) > 0 {
		say("It uses other containers' volumes (--volumes-from %s), which hlm doesn't keep.", strings.Join(x.VolumesFrom, ", "))
	}
	if x.CgroupParent != "" {
		say("Its cgroup parent (%s) wouldn't be kept.", x.CgroupParent)
	}
	if x.MemoryReservation > 0 {
		say("Its memory reservation wouldn't be kept.")
	}
	if x.CPUShares > 0 && x.CPUShares != 1024 {
		say("Its CPU shares (%d) wouldn't be kept.", x.CPUShares)
	}
	if x.BlkioWeight > 0 {
		say("Its disk I/O weight wouldn't be kept.")
	}
	if x.OomKillDisable != nil && *x.OomKillDisable {
		say("It wouldn't be kept from being killed when memory runs out (--oom-kill-disable).")
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func netName(mode string) string {
	if mode == "" || mode == "default" {
		return "bridge"
	}
	return mode
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
