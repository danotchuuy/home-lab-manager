package stack

import (
	"archive/tar"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/danotchuuy/home-lab-manager/internal/compose"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// Labels Docker Compose puts on the containers it makes.
const (
	composeProject     = "com.docker.compose.project"
	composeService     = "com.docker.compose.service"
	composeConfigFiles = "com.docker.compose.project.config_files"
	composeWorkingDir  = "com.docker.compose.project.working_dir"
	composeNumber      = "com.docker.compose.container-number"
)

// composeFiles are the names Compose looks for in a folder, in its order.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// maxFile is the largest Compose file or .env read from a host.
const maxFile = 512 << 10

// helperImage is what reads files on a host when none of the stack's own
// images is there.
const helperImage = "busybox:1.37"

// Found is a Compose stack on a host: its file, and the containers Compose
// made for it.
type Found struct {
	// Name is the stack's name: Compose's project name.
	Name string `json:"name"`
	Host string `json:"host"`
	// Source is the Compose file, Dir its folder.
	Source string `json:"source"`
	Dir    string `json:"dir"`
	YAML   string `json:"yaml"`
	Env    string `json:"env,omitempty"`
	// Containers are the containers Compose made, by service.
	Containers map[string]Member `json:"containers"`
	Warnings   []string          `json:"warnings"`
	// Pinned are the services whose latest images are changed to the
	// versions they are, by service.
	Pinned map[string]Pin `json:"pinned,omitempty"`
}

// AdoptPlan is what adopting a stack would do.
type AdoptPlan struct {
	Found Found `json:"found"`
	Plan  Plan  `json:"plan"`
}

// Find reads the Compose file at file on a host (or the one Compose would
// use in that folder) and its .env, and finds the containers Compose made
// for it. The stack's name is Compose's project name: its networks and
// volumes are named after it, so another would leave them behind.
func (s *Service) Find(ctx context.Context, host, file string) (Found, error) {
	host = cmp.Or(host, manager.LocalHost)
	file = strings.TrimSpace(file)
	if !strings.HasPrefix(file, "/") {
		return Found{}, errors.New("give the Compose file's full path on the host, such as /opt/stacks/app/docker-compose.yml")
	}
	file = path.Clean(file)
	ctx = onHost(ctx, host)
	d := s.docker(ctx)
	all, err := d.ListContainers(ctx)
	if err != nil {
		return Found{}, err
	}
	f := Found{Host: host, Containers: map[string]Member{}, Warnings: []string{}}
	if f.Source, f.YAML, f.Env, err = s.readCompose(ctx, d, file, all); err != nil {
		return Found{}, err
	}
	f.Dir = path.Dir(f.Source)

	// Compose's containers for it: those whose labels name the file, or
	// its folder.
	project := ""
	for _, c := range all {
		if c.Labels[composeProject] == "" || c.Labels[compose.LabelStack] != "" {
			continue
		}
		files := strings.Split(c.Labels[composeConfigFiles], ",")
		if slices.Contains(files, f.Source) || (c.Labels[composeConfigFiles] == "" && c.Labels[composeWorkingDir] == f.Dir) {
			if len(files) > 1 {
				return Found{}, fmt.Errorf("Compose runs %s from several files (%s): hlm keeps one, so merge them into one file first",
					c.Labels[composeProject], strings.Join(files, ", "))
			}
			project = c.Labels[composeProject]
			break
		}
	}
	project = cmp.Or(project, projectName(f.YAML, f.Dir))
	f.Name = project
	if !compose.ValidName.MatchString(f.Name) {
		return Found{}, fmt.Errorf("Compose's project name %q can't be a stack's name in hlm (lower-case letters, digits, dashes and underscores)", f.Name)
	}
	for _, c := range all {
		if c.Labels[composeProject] != project || c.Labels[compose.LabelStack] != "" {
			continue
		}
		svc := c.Labels[composeService]
		m := Member{Service: svc, ID: c.ID, State: c.State, Status: c.Status, Image: c.Image}
		if len(c.Names) > 0 {
			m.Container = strings.TrimPrefix(c.Names[0], "/")
		}
		m.Server = qualify(ctx, m.Container)
		if other, dup := f.Containers[svc]; dup {
			return Found{}, fmt.Errorf("service %s has more than one container (%s and %s): hlm runs one per service, so scale it to one first",
				svc, other.Container, m.Container)
		}
		f.Containers[svc] = m
	}
	if p, err := compose.Parse([]byte(f.YAML), f.Env, f.Name); err == nil {
		for _, svc := range sortedNames(f.Containers) {
			if p.Service(svc) == nil {
				f.Warnings = append(f.Warnings, fmt.Sprintf("%s is from a service that's no longer in the file; it's left as it is", f.Containers[svc].Container))
				delete(f.Containers, svc)
			}
		}
	}
	return f, nil
}

// projectName is the name Compose gives a project with no containers yet:
// the file's name:, or its folder's name made into one.
func projectName(yml, dir string) string {
	var top struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal([]byte(yml), &top) == nil && top.Name != "" && !strings.Contains(top.Name, "$") {
		return top.Name
	}
	n := strings.ToLower(path.Base(dir))
	n = notInName.ReplaceAllString(n, "")
	return strings.TrimLeft(n, "-_")
}

var notInName = regexp.MustCompile(`[^a-z0-9_-]`)

// AdoptPreview says what adopting the Compose stack at file would do.
func (s *Service) AdoptPreview(ctx context.Context, host, file string) (AdoptPlan, error) {
	f, def, err := s.adoptable(ctx, host, file)
	if err != nil {
		return AdoptPlan{}, err
	}
	plan, err := s.preview(ctx, f.Name, def, f.Containers)
	if err != nil {
		return AdoptPlan{}, err
	}
	plan.Warnings = append(plan.Warnings, f.Warnings...)
	markPinned(&plan, f.Pinned)
	return AdoptPlan{Found: f, Plan: plan}, nil
}

// Adopt makes the Compose stack at file one of hlm's: its file and .env
// are kept as the stack's, and each container Compose made is replaced
// with hlm's own (same name, image, folders and networks), starting those
// that aren't there. Compose's networks and volumes are used as they are.
func (s *Service) Adopt(ctx context.Context, host, file string, emit func(Event)) error {
	f, def, err := s.adoptable(ctx, host, file)
	if err != nil {
		return err
	}
	if err := s.claim(f.Name); err != nil {
		return err
	}
	defer s.release(f.Name)
	if _, err := s.Definition(f.Name); err == nil {
		return fmt.Errorf("a stack called %s exists already", f.Name)
	}
	plan, err := s.preview(ctx, f.Name, def, f.Containers)
	if err != nil {
		return err
	}
	plan.Warnings = append(plan.Warnings, f.Warnings...)
	emit(Event{Stage: "create", Message: fmt.Sprintf("Adopting %s from %s", f.Name, f.Source)})
	for _, svc := range sortedNames(f.Pinned) {
		emit(Event{Stage: "create", Message: fmt.Sprintf("%s: %s is version %s; the stack uses that, and follows %s when it's updated", svc, f.Pinned[svc].From, f.Pinned[svc].To, f.Pinned[svc].Tag)})
	}
	return s.run(ctx, f.Name, def, plan, emit)
}

// adoptable finds a stack to adopt, and its definition.
func (s *Service) adoptable(ctx context.Context, host, file string) (Found, Definition, error) {
	f, err := s.Find(ctx, host, file)
	if err != nil {
		return Found{}, Definition{}, err
	}
	if _, err := s.Definition(f.Name); err == nil {
		return Found{}, Definition{}, fmt.Errorf("hlm has a stack called %s already", f.Name)
	}
	def := Definition{Host: f.Host, YAML: f.YAML, Env: f.Env, Source: f.Source, Dir: f.Dir, Names: map[string]string{}}
	if def.Host == manager.LocalHost {
		def.Host = ""
	}
	for svc, m := range f.Containers {
		def.Names[svc] = m.Container
	}
	// Services not running yet get the names Compose would give them, so
	// they match if Compose is used on it again.
	if p, err := compose.Parse([]byte(f.YAML), f.Env, f.Name); err == nil {
		for _, svc := range p.Services {
			if _, ok := def.Names[svc.Name]; !ok && svc.ContainerName == "" {
				def.Names[svc.Name] = f.Name + "-" + svc.Name + "-1"
			}
		}
	}
	// Latest (and stable) images get their versions.
	pins, warnings := s.pinMoving(onHost(ctx, f.Host), f.Name, f.Containers, &def)
	f.Warnings = append(f.Warnings, warnings...)
	if len(pins) > 0 {
		f.Pinned = pins
	}
	return f, def, nil
}

// markPinned notes the services whose images were pinned in a plan.
func markPinned(plan *Plan, pins map[string]Pin) {
	for i := range plan.Services {
		if p, ok := pins[plan.Services[i].Service]; ok {
			plan.Services[i].Pinned = &p
		}
	}
}

// readCompose reads a Compose file on the host (file, or the one Compose
// would use in the folder file), and the .env beside it, through a
// container that's created, never started, with the folder mounted
// read-only.
func (s *Service) readCompose(ctx context.Context, d Docker, file string, all []docker.ContainerSummary) (src, yml, env string, err error) {
	// Any image that's there will do, as the container never runs: one of
	// the host's own, or the helper's.
	img := ""
	for _, c := range all {
		if c.Image != "" && !strings.HasPrefix(c.Image, "sha256:") {
			img = c.Image
			break
		}
	}
	if img == "" {
		img = helperImage
		if _, err := d.InspectImage(ctx, img); err != nil {
			if err := d.PullImage(ctx, img, nil); err != nil {
				return "", "", "", fmt.Errorf("pull %s to read the file: %w", img, err)
			}
		}
	}
	read := func(dir string, names ...string) (string, string, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		id, _, err := d.CreateContainer(ctx, "", docker.CreateRequest{
			Image: img, Cmd: []string{"true"}, Entrypoint: []string{},
			Labels: map[string]string{"io.hlm.helper": "compose-read"},
			HostConfig: docker.HostConfig{NetworkMode: "none",
				Mounts: []docker.Mount{{Type: "bind", Source: dir, Target: "/hlm-src", ReadOnly: true}}},
		})
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not a directory") {
				return "", "", fmt.Errorf("%s isn't a folder on the host", dir)
			}
			return "", "", fmt.Errorf("read %s on the host: %w", dir, err)
		}
		defer d.RemoveContainer(context.WithoutCancel(ctx), id, true)
		for _, n := range names {
			data, ok, err := copyFile(ctx, d, id, "/hlm-src/"+n)
			if err != nil {
				return "", "", fmt.Errorf("read %s: %w", path.Join(dir, n), err)
			}
			if ok {
				return n, data, nil
			}
		}
		return "", "", nil
	}

	// file is the Compose file, or a folder with one.
	dir, base := path.Dir(file), path.Base(file)
	found, data, err := read(dir, base)
	if err != nil {
		return "", "", "", err
	}
	if found == "" {
		// A folder: Compose's own file in it.
		if found, data, err = read(file, composeFiles...); err != nil {
			if !strings.Contains(err.Error(), "isn't a folder") {
				return "", "", "", err
			}
			return "", "", "", fmt.Errorf("there's no %s on the host", file)
		}
		if found == "" {
			return "", "", "", fmt.Errorf("there's no Compose file (%s) in %s", strings.Join(composeFiles, ", "), file)
		}
		dir = file
	}
	src = path.Join(dir, found)
	_, env, err = read(dir, ".env")
	if err != nil {
		return "", "", "", err
	}
	return src, data, env, nil
}

// copyFile reads a regular file from a container; ok is false if there's
// none at p (nothing, or a folder).
func copyFile(ctx context.Context, d Docker, id, p string) (data string, ok bool, err error) {
	rc, _, err := d.CopyFrom(ctx, id, p)
	if err != nil {
		if docker.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	h, err := tr.Next()
	if err != nil {
		return "", false, err
	}
	if h.Typeflag != tar.TypeReg {
		return "", false, nil
	}
	if h.Size > maxFile {
		return "", false, fmt.Errorf("it's bigger than %d KB", maxFile>>10)
	}
	b, err := io.ReadAll(io.LimitReader(tr, maxFile))
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}
