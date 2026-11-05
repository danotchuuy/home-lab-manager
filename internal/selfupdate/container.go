package selfupdate

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// HelperCommand is the hlm subcommand the helper container runs.
const HelperCommand = "selfupdate-helper"

// labelHelper marks helper containers so they can be cleaned up.
const labelHelper = "io.hlm.selfupdate-helper"

var containerIDInMount = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// ownContainerID finds the ID of the container hlm runs in, from the files
// Docker bind-mounts into every container (/etc/hostname and friends live
// under /var/lib/docker/containers/<id>/). It returns "" outside a container.
func ownContainerID() string {
	for _, key := range []string{"HLM_CONTAINER_ID", "GSH_CONTAINER_ID"} { // GSH_ from before the rename
		if id := os.Getenv(key); id != "" {
			return id
		}
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	return containerIDFromMountinfo(string(data))
}

func containerIDFromMountinfo(mountinfo string) string {
	sc := bufio.NewScanner(strings.NewReader(mountinfo))
	for sc.Scan() {
		line := sc.Text()
		// Only the mounts Docker makes for this container's own files.
		if !strings.Contains(line, " /etc/hostname ") && !strings.Contains(line, " /etc/hosts ") &&
			!strings.Contains(line, " /etc/resolv.conf ") {
			continue
		}
		if m := containerIDInMount.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// imageFor returns the image reference to update to. A container that
// follows a moving tag of the release image (e.g. :latest, as in the compose
// file) keeps that tag, so `docker compose up` doesn't switch it back later;
// one pinned to a version, or running some other image (e.g. built
// locally), moves to the release's version tag.
func (u *Updater) imageFor(rel *Release) string {
	name, tag := docker.SplitImageRef(u.image)
	if sameRepo(name, u.cfg.Image) && !strings.Contains(u.image, "@") {
		if _, pinned := parseVersion(tag); !pinned {
			return u.image
		}
	}
	return u.cfg.Image + ":" + rel.Version
}

// sameRepo reports whether two image names refer to the same repository,
// treating Docker Hub's short and long forms alike: "owner/name",
// "docker.io/owner/name" and "index.docker.io/owner/name".
func sameRepo(a, b string) bool {
	return normalizeRepo(a) == normalizeRepo(b)
}

func normalizeRepo(name string) string {
	name = strings.ToLower(name)
	for _, prefix := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name // official images
	}
	return name
}

// versionLabel is the OCI label release images carry their version in.
const versionLabel = "org.opencontainers.image.version"

func (u *Updater) applyContainer(ctx context.Context, rel *Release, emit func(Event)) error {
	d := u.cfg.Docker
	image := u.imageFor(rel)

	emit(Event{Stage: "pull", Message: "Pulling " + image})
	if err := d.PullImage(ctx, image, func(p docker.PullProgress) {
		emit(Event{Stage: "pull", Progress: &p})
	}); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	img, err := d.InspectImage(ctx, image)
	if err != nil {
		return err
	}
	if len(img.Config.Entrypoint) == 0 {
		return fmt.Errorf("image %s has no entrypoint; it doesn't look like a hlm image", image)
	}
	if v := img.Config.Labels[versionLabel]; v != "" && v != rel.Version {
		return fmt.Errorf("%s is still %s, not %s; the new image may not be published yet, so try again in a few minutes", image, v, rel.Version)
	}

	// The helper is a copy of this container (same mounts, Docker access and
	// environment) running the new image's helper command instead of the
	// server, with no ports and no restart policy.
	raw, err := d.InspectContainerRaw(ctx, u.containerID)
	if err != nil {
		return err
	}
	// The pull may have moved the tag off hlm's current image, so use the
	// copy of its config read at startup, and hand it to the helper too.
	oldImg := *u.ownImage
	oldJSON, _ := json.Marshal(oldImg)
	body := helperConfig(raw, oldImg, image, img, []string{HelperCommand, u.containerID, image, u.cfg.Version, rel.Version,
		base64.StdEncoding.EncodeToString(oldJSON)})

	name := fmt.Sprintf("hlm-selfupdate-%d", time.Now().Unix())
	emit(Event{Stage: "install", Message: "Starting update helper " + name})
	id, err := d.CreateContainerRaw(ctx, name, body)
	if err != nil {
		return fmt.Errorf("create update helper: %w", err)
	}
	if err := d.StartContainer(ctx, id); err != nil {
		_ = d.RemoveContainer(context.Background(), id, true)
		return fmt.Errorf("start update helper: %w", err)
	}
	emit(Event{Stage: "restart", Message: fmt.Sprintf("The helper is replacing this container with %s; hlm will be back in a moment", rel.Version)})
	return nil
}

// RunHelper replaces the hlm container oldID with one running newImage and
// the same configuration. If the new container doesn't stay up, the old one
// is restored. It runs in the helper container started by applyContainer.
// oldImage is the base64 JSON of the old image (see Updater.ownImage).
func RunHelper(ctx context.Context, d *docker.Client, dataDir, oldID, newImage, from, to, oldImage string, log func(string, ...any)) error {
	resultPath := (&Updater{cfg: Config{DataDir: dataDir}}).resultPath()
	fail := func(err error) error {
		log("update failed: %v", err)
		_ = writeResult(resultPath, Result{From: from, To: to, Error: err.Error(), At: time.Now()})
		return err
	}

	// Let the old hlm finish answering the request that started us.
	time.Sleep(3 * time.Second)

	raw, err := d.InspectContainerRaw(ctx, oldID)
	if err != nil {
		return fail(fmt.Errorf("inspect old container: %w", err))
	}
	name := strings.TrimPrefix(str(raw, "Name"), "/")
	var oldImg docker.Image
	data, err := base64.StdEncoding.DecodeString(oldImage)
	if err == nil {
		err = json.Unmarshal(data, &oldImg)
	}
	if err != nil {
		return fail(fmt.Errorf("decode old image config: %w", err))
	}
	newImg, err := d.InspectImage(ctx, newImage)
	if err != nil {
		return fail(fmt.Errorf("inspect new image: %w", err))
	}
	body, extraNets := cloneConfig(raw, oldImg, newImage, newImg)

	log("stopping %s", name)
	if err := d.StopContainer(ctx, oldID, 30); err != nil {
		return fail(fmt.Errorf("stop old container: %w", err))
	}
	backup := fmt.Sprintf("%s-old-%d", name, time.Now().Unix())
	if err := d.RenameContainer(ctx, oldID, backup); err != nil {
		_ = d.StartContainer(ctx, oldID)
		return fail(fmt.Errorf("rename old container: %w", err))
	}

	restore := func(newID string, cause error) error {
		// Record the failure first, so the restored hlm finds it on start.
		err := fail(cause)
		rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if newID != "" {
			_ = d.RemoveContainer(rctx, newID, true)
		}
		if err := d.RenameContainer(rctx, oldID, name); err != nil {
			log("restore name: %v", err)
		}
		if err := d.StartContainer(rctx, oldID); err != nil {
			log("restart old container: %v", err)
		}
		return err
	}

	log("creating %s from %s", name, newImage)
	newID, err := d.CreateContainerRaw(ctx, name, body)
	if err != nil {
		return restore("", fmt.Errorf("create new container: %w", err))
	}
	for netName, ep := range extraNets {
		if err := d.ConnectNetwork(ctx, netName, newID, ep); err != nil {
			return restore(newID, fmt.Errorf("connect network %s: %w", netName, err))
		}
	}
	if err := d.StartContainer(ctx, newID); err != nil {
		return restore(newID, fmt.Errorf("start new container: %w", err))
	}

	// Watch the new container for a while; a crash or restart loop means the
	// new version doesn't work here.
	log("waiting for the new container to settle")
	for i := 0; i < 20; i++ {
		time.Sleep(time.Second)
		info, err := d.InspectContainer(ctx, newID)
		if err != nil {
			return restore(newID, fmt.Errorf("inspect new container: %w", err))
		}
		if !info.State.Running || info.State.Restarting {
			return restore(newID, fmt.Errorf("the new container stopped (exit code %d)", info.State.ExitCode))
		}
	}

	_ = writeResult(resultPath, Result{From: from, To: to, OK: true, At: time.Now()})
	if err := d.RemoveContainer(ctx, oldID, true); err != nil {
		log("remove old container %s: %v", backup, err)
	}
	log("updated %s to %s", name, to)
	return nil
}

// helperConfig is the create request for the update helper: a copy of
// hlm's container (same mounts, Docker access and environment) running cmd
// on the new image, with no ports, no restart policy, and nothing that has
// to be unique to a container: hlm is still running while the helper
// starts, so a fixed MAC or IP address would be taken.
func helperConfig(raw map[string]any, oldImg docker.Image, image string, img docker.Image, cmd []string) map[string]any {
	body, _ := cloneConfig(raw, oldImg, image, img)
	body["Entrypoint"] = img.Config.Entrypoint
	body["Cmd"] = cmd
	labels, _ := body["Labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	for k := range labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			delete(labels, k) // don't let compose think the helper is its service
		}
	}
	labels[labelHelper] = "true"
	body["Labels"] = labels
	delete(body, "ExposedPorts")
	delete(body, "Healthcheck")
	// Older API versions report a fixed MAC address (compose's mac_address,
	// docker run --mac-address) here as well as on the network.
	delete(body, "MacAddress")
	hc, _ := body["HostConfig"].(map[string]any)
	delete(hc, "PortBindings")
	hc["RestartPolicy"] = map[string]any{"Name": "no"}
	// The default endpoint is enough to reach Docker; without the network
	// settings, it has no fixed IP or MAC address.
	delete(body, "NetworkingConfig")
	return body
}

// cloneConfig turns a container's inspect output into a create request for
// the same container on newImage. Values the old container only had because
// its image set them (environment, labels, command, ...) are dropped, so the
// new image's defaults apply. It returns the body and any networks beyond the
// first, which must be connected after creation.
func cloneConfig(raw map[string]any, oldImg docker.Image, newImage string, newImg docker.Image) (map[string]any, map[string]any) {
	body := deepCopy(raw["Config"]).(map[string]any)
	body["Image"] = newImage
	id := str(raw, "Id")
	if h, _ := body["Hostname"].(string); h != "" && strings.HasPrefix(id, h) {
		delete(body, "Hostname") // Docker's default: the container ID
	}

	if env, ok := body["Env"].([]any); ok {
		var keep []any
		for _, e := range env {
			if s, _ := e.(string); !slices.Contains(oldImg.Config.Env, s) {
				keep = append(keep, e)
			}
		}
		body["Env"] = keep
	}
	if labels, ok := body["Labels"].(map[string]any); ok {
		for k, v := range labels {
			if old, inImage := oldImg.Config.Labels[k]; inImage && v == old {
				delete(labels, k)
			}
		}
	}
	for key, imgVal := range map[string][]string{"Cmd": oldImg.Config.Cmd, "Entrypoint": oldImg.Config.Entrypoint} {
		if equalStrings(body[key], imgVal) {
			delete(body, key)
		}
	}
	if s, _ := body["WorkingDir"].(string); s == oldImg.Config.WorkingDir {
		delete(body, "WorkingDir")
	}
	if s, _ := body["User"].(string); s == oldImg.Config.User {
		delete(body, "User")
	}

	hc := deepCopy(raw["HostConfig"]).(map[string]any)
	body["HostConfig"] = hc

	extra := map[string]any{}
	mode, _ := hc["NetworkMode"].(string)
	settings, _ := raw["NetworkSettings"].(map[string]any)
	nets, _ := settings["Networks"].(map[string]any)
	primary := mode
	if primary == "default" {
		primary = "bridge"
	}
	for netName, v := range nets {
		ep, _ := v.(map[string]any)
		clean := map[string]any{}
		for _, k := range []string{"IPAMConfig", "Links", "DriverOpts", "MacAddress"} {
			if val, ok := ep[k]; ok && val != nil && val != "" {
				clean[k] = val
			}
		}
		if aliases, ok := ep["Aliases"].([]any); ok {
			var keep []any
			for _, a := range aliases {
				if s, _ := a.(string); s != "" && !strings.HasPrefix(id, s) {
					keep = append(keep, a) // drop the old container's ID alias
				}
			}
			if len(keep) > 0 {
				clean["Aliases"] = keep
			}
		}
		if netName == primary {
			body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{netName: clean}}
		} else if mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:") {
			extra[netName] = clean
		}
	}
	return body, extra
}

func equalStrings(v any, want []string) bool {
	list, ok := v.([]any)
	if !ok || len(list) != len(want) {
		return v == nil && len(want) == 0
	}
	for i, x := range list {
		if s, _ := x.(string); s != want[i] {
			return false
		}
	}
	return true
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func deepCopy(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	if out == nil {
		return map[string]any{}
	}
	return out
}

// CleanupHelpers removes finished update helper containers.
func CleanupHelpers(ctx context.Context, d *docker.Client) error {
	list, err := d.ListContainers(ctx, labelHelper+"=true")
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range list {
		if c.State == "running" {
			continue
		}
		if err := d.RemoveContainer(ctx, c.ID, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
