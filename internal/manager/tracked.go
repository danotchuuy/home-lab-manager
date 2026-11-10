package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/imagetag"
)

// Stacks' services can follow a tag (latest) from one version to the next:
// their file names a version, and the update check reports the version the
// tag has moved on to. Updating such a server updates its stack, which
// changes the file to it.

// StackHooks are what the manager needs from Compose stacks.
type StackHooks struct {
	// Tracks is the tag a stack's service follows, or "".
	Tracks func(stack, service string) string
	// Update updates a stack, as its Update button does.
	Update func(ctx context.Context, stack string, emit func(Event)) error
	// VersionOf is the version tag of image's repository with digest.
	VersionOf func(ctx context.Context, image, digest string) (string, error)
}

// UseStacks connects the manager to Compose stacks.
func (m *Manager) UseStacks(h StackHooks) { m.stacks = h }

// checkTracked is the update status of a stack's server that follows a
// tag: a newer version when the tag has moved on from the version it
// runs. Otherwise st, the status of its own tag, stands.
func (m *Manager) checkTracked(ctx context.Context, srv Server, st UpdateStatus,
	remoteDigest func(ctx context.Context, host, ref string) (string, error)) UpdateStatus {
	if srv.Stack == "" || m.stacks.Tracks == nil || m.stacks.VersionOf == nil {
		return st
	}
	follow := m.stacks.Tracks(srv.Stack, srv.StackService)
	ref, ok := imagetag.Parse(srv.Image)
	if follow == "" || !ok || ref.Tag == follow {
		return st
	}
	host := HostOf(srv.ID)
	remote, err := remoteDigest(ctx, host, ref.WithTag(follow))
	if err != nil {
		m.log.Debug("look up a followed tag", "server", srv.Name, "image", ref.WithTag(follow), "err", err)
		return st
	}
	img, err := m.docker.InspectImage(ctx, host, cmpOr(srv.imageID, srv.Image))
	if err == nil && imagetag.RepoDigest(ref, img.RepoDigests) == remote {
		return st // it runs what the tag is
	}
	tag, err := m.stacks.VersionOf(ctx, ref.String(), remote)
	if err != nil || tag == ref.Tag {
		if err != nil {
			m.log.Debug("find a followed tag's version", "server", srv.Name, "image", ref.WithTag(follow), "err", err)
		}
		return st
	}
	return UpdateStatus{State: UpdateAvailable, Version: tag, CheckedAt: st.CheckedAt,
		Reason: fmt.Sprintf("version %s is out (%s is now %s); updating the stack changes its file to it", tag, follow, tag)}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// updateStack updates the stack a server is part of, for Update.
func (m *Manager) updateStack(ctx context.Context, name, stack, id string, emit func(Event)) (Server, error) {
	emit(Event{Stage: "pull", Message: fmt.Sprintf("%s is part of the stack %s: updating the stack", name, stack)})
	if err := m.stacks.Update(ctx, stack, emit); err != nil {
		return Server{}, err
	}
	m.updates.forget(id)
	return m.Get(ctx, strings.TrimPrefix(name, "/"))
}
