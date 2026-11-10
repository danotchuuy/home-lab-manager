package stack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/compose"
	"github.com/danotchuuy/home-lab-manager/internal/imagetag"
)

// A stack adopted with images tagged latest (or stable) gets the version
// each one is instead, so the file says what runs; so does a stack already
// adopted, when asked (Pin). The services are marked as following their
// tag (Definition.Track): the update check reports the version it has
// moved on to, and updating the stack changes the file to it.

// Versions finds images' version tags; see imagetag.Finder.
type Versions interface {
	Digest(ctx context.Context, image string) (string, error)
	VersionOf(ctx context.Context, image, digest string) (string, error)
}

// UseVersions sets how version tags are found. Without it, adopted images
// keep their latest tags.
func (s *Service) UseVersions(v Versions) { s.versions = v }

// Follow is a service's image following a tag: hlm set the file to Image
// at Version, the version Tag was. Changing the image by hand stops it.
type Follow struct {
	Image   string `json:"image"` // as written, without its tag
	Tag     string `json:"tag"`   // followed: latest
	Version string `json:"version"`
}

// followed are the follows still in force in a Compose file: those whose
// image is still the one hlm set.
func followed(yml string, track map[string]Follow) map[string]Follow {
	var out map[string]Follow
	for svc, f := range track {
		written, err := compose.ServiceImage(yml, svc)
		ref, ok := imagetag.Parse(written)
		if err != nil || !ok || ref.Name != f.Image || ref.Tag != f.Version {
			continue
		}
		if out == nil {
			out = map[string]Follow{}
		}
		out[svc] = f
	}
	return out
}

// Followable says whether a tag moves from one version to the next in a
// way that can be followed: latest, and stable.
func Followable(tag string) bool { return tag == imagetag.Latest || tag == "stable" }

// Pin is a service's image changed from a tag it follows (latest) to its
// version.
type Pin struct {
	From string `json:"from"`
	To   string `json:"to"`
	Tag  string `json:"tag"` // followed
}

// Tracks says which tag a stack's service follows from version to version
// ("" when it follows none): its update check and updates go by it.
func (s *Service) Tracks(stack, service string) string {
	d, err := s.Definition(stack)
	if err != nil {
		return ""
	}
	return followed(d.YAML, d.Track)[service].Tag
}

// pinMoving changes the definition's images tagged latest (or stable) to
// their versions: the version of the image a service's container runs, or,
// for one not running, the one the tag is now. Images it can't find a
// version for keep their tag, with a warning. running are the services'
// containers.
func (s *Service) pinMoving(ctx context.Context, name string, running map[string]Member, def *Definition) (map[string]Pin, []string) {
	if s.versions == nil {
		return nil, nil
	}
	p, err := compose.Parse([]byte(def.YAML), def.Env, name)
	if err != nil {
		return nil, nil // the preview reports it
	}
	names := make([]string, 0, len(p.Services))
	for _, svc := range p.Services {
		names = append(names, svc.Name)
	}
	sort.Strings(names)
	pins := map[string]Pin{}
	var warnings []string
	for _, svc := range names {
		written, err := compose.ServiceImage(def.YAML, svc)
		if err != nil || written == "" {
			continue
		}
		ref, ok := imagetag.Parse(written)
		if !ok || !Followable(ref.Tag) {
			continue // a version already, or made from variables
		}
		tag, err := s.versionNow(ctx, ref, running[svc])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("service %s: couldn't find which version %s is (%v), so it stays on %s", svc, written, err, ref.Tag))
			continue
		}
		yml, err := compose.SetServiceImage(def.YAML, svc, ref.WithTag(tag))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("service %s: %v, so it stays on %s", svc, err, ref.Tag))
			continue
		}
		def.YAML = yml
		if def.Track == nil {
			def.Track = map[string]Follow{}
		}
		def.Track[svc] = Follow{Image: ref.Name, Tag: ref.Tag, Version: tag}
		pins[svc] = Pin{From: written, To: ref.WithTag(tag), Tag: ref.Tag}
	}
	return pins, warnings
}

// versionNow is the version a service runs: that of its container's image,
// or, without one, the version its tag is.
func (s *Service) versionNow(ctx context.Context, ref imagetag.Ref, m Member) (string, error) {
	digest := ""
	if m.ID != "" {
		info, err := s.docker(ctx).InspectContainer(ctx, m.ID)
		if err != nil {
			return "", err
		}
		img, err := s.docker(ctx).InspectImage(ctx, info.Image)
		if err != nil {
			return "", err
		}
		if digest = imagetag.RepoDigest(ref, img.RepoDigests); digest == "" {
			return "", errors.New("its image wasn't pulled from the registry")
		}
	} else {
		var err error
		if digest, err = s.versions.Digest(ctx, ref.String()); err != nil {
			return "", err
		}
	}
	tag, err := s.versions.VersionOf(ctx, ref.String(), digest)
	if errors.Is(err, imagetag.ErrNoVersion) && m.ID != "" {
		return "", errors.New("no version tag has the image it runs; it may be older than the registry keeps tags for")
	}
	return tag, err
}

// followTracked changes a definition's services that follow latest to the
// version latest is now, saying what changed. It's done before updating.
func (s *Service) followTracked(ctx context.Context, name string, def *Definition, emit func(Event)) (bool, error) {
	if s.versions == nil || len(def.Track) == 0 {
		return false, nil
	}
	changed := false
	track := followed(def.YAML, def.Track)
	for _, svc := range sortedNames(track) {
		fl := track[svc]
		follow := fl.Tag
		written, _ := compose.ServiceImage(def.YAML, svc)
		ref, _ := imagetag.Parse(written)
		digest, err := s.versions.Digest(ctx, ref.WithTag(follow))
		if err != nil {
			emit(Event{Stage: "warning", Message: fmt.Sprintf("%s: couldn't look up %s (%v); updating %s as it is", svc, ref.WithTag(follow), err, written)})
			continue
		}
		tag, err := s.versions.VersionOf(ctx, ref.String(), digest)
		if err != nil {
			emit(Event{Stage: "warning", Message: fmt.Sprintf("%s: couldn't find which version %s is now (%v); updating %s as it is", svc, ref.WithTag(follow), err, written)})
			continue
		}
		if tag == ref.Tag {
			continue
		}
		yml, err := compose.SetServiceImage(def.YAML, svc, ref.WithTag(tag))
		if err != nil {
			return false, fmt.Errorf("%s: %w", svc, err)
		}
		def.YAML = yml
		fl.Version = tag
		track[svc] = fl
		changed = true
		emit(Event{Stage: "pull", Message: fmt.Sprintf("%s: %s is now version %s (was %s)", svc, ref.WithTag(follow), tag, ref.Tag)})
	}
	if changed {
		def.Track = track
		if err := s.save(name, def); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// Pinnable are a stack's services whose images are tagged latest (or
// stable) in its file, so Pin would set them to their versions.
func Pinnable(d Definition, name string) []string {
	p, err := compose.Parse([]byte(d.YAML), d.Env, name)
	if err != nil {
		return nil
	}
	var out []string
	for _, svc := range p.Services {
		written, err := compose.ServiceImage(d.YAML, svc.Name)
		if ref, ok := imagetag.Parse(written); err == nil && ok && Followable(ref.Tag) {
			out = append(out, svc.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Pin sets a stack's images tagged latest (or stable) to the versions its
// containers run, as adopting does, and has them follow their tag. Only
// the file changes: the containers run the same images, and are recreated
// with the version's tag when the stack is next updated or applied.
func (s *Service) Pin(ctx context.Context, name string, emit func(Event)) error {
	if s.versions == nil {
		return errors.New("hlm can't look up images' versions")
	}
	d, err := s.Definition(name)
	if err != nil {
		return err
	}
	ctx = onHost(ctx, d.Host)
	if err := s.claim(name); err != nil {
		return err
	}
	defer s.release(name)
	running, err := s.members(ctx, name)
	if err != nil {
		return err
	}
	pins, warnings := s.pinMoving(ctx, name, running, &d)
	for _, w := range warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	if len(pins) == 0 {
		emit(Event{Stage: "done", Message: "No image was changed"})
		return nil
	}
	// The plan must still read, as it did before.
	if _, err := s.preview(ctx, name, d, nil); err != nil {
		return err
	}
	d.Updated = time.Now().UTC()
	if err := s.save(name, &d); err != nil {
		return err
	}
	for _, svc := range sortedNames(pins) {
		p := pins[svc]
		emit(Event{Stage: "create", Message: fmt.Sprintf("%s: %s is version %s; the stack uses that, and follows %s when it's updated", svc, p.From, p.To, p.Tag)})
	}
	emit(Event{Stage: "done", Message: fmt.Sprintf("Pinned %d of the stack's images to their versions", len(pins))})
	return nil
}
