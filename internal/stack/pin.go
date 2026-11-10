package stack

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/danotchuuy/home-lab-manager/internal/compose"
	"github.com/danotchuuy/home-lab-manager/internal/imagetag"
)

// A stack adopted with images tagged latest gets the version each one is
// instead, so the file says what runs. The services are marked as
// following latest (Definition.Track): the update check reports the
// version latest has moved on to, and updating the stack changes the file
// to it.

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

// Pin is a service's image changed from latest to its version.
type Pin struct {
	From string `json:"from"`
	To   string `json:"to"`
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

// pinLatest changes the definition's latest images to their versions: the
// version of the image a service's container runs, or, for one not
// running, the one latest is now. Images it can't find a version for keep
// latest, with a warning.
func (s *Service) pinLatest(ctx context.Context, f Found, def *Definition) (map[string]Pin, []string) {
	if s.versions == nil {
		return nil, nil
	}
	p, err := compose.Parse([]byte(def.YAML), def.Env, f.Name)
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
		if !ok || ref.Tag != imagetag.Latest {
			continue // a version already, or made from variables
		}
		tag, err := s.versionNow(ctx, ref, f.Containers[svc])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("service %s: couldn't find which version %s is (%v), so it stays on latest", svc, written, err))
			continue
		}
		yml, err := compose.SetServiceImage(def.YAML, svc, ref.WithTag(tag))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("service %s: %v, so it stays on latest", svc, err))
			continue
		}
		def.YAML = yml
		if def.Track == nil {
			def.Track = map[string]Follow{}
		}
		def.Track[svc] = Follow{Image: ref.Name, Tag: imagetag.Latest, Version: tag}
		pins[svc] = Pin{From: written, To: ref.WithTag(tag)}
	}
	return pins, warnings
}

// versionNow is the version a service runs: that of its container's image,
// or, without one, the version latest is.
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
