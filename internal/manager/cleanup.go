package manager

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Disk cleanup: what can be deleted on a Docker host to free space, and
// deleting it. The rules keep it safe: an image any container uses (running
// or not, hlm's or not) is never a candidate; images are deleted without
// force, so Docker refuses one that's in use after all; volumes that a
// deleted server's record keeps are left to the Host page's Deleted
// servers; and a recreated server's leftover "-old-" container is only one
// if the server it was renamed from exists again.

// Kinds of cleanup items.
const (
	CleanOldImage    = "old-image"    // untagged: left behind when an update moved the tag
	CleanUnusedImage = "unused-image" // tagged, but no container uses it
	CleanHelper      = "helper"       // a stopped container hlm left behind
	CleanBuildCache  = "build-cache"
	CleanVolume      = "volume" // no container uses it, and no deleted server's record keeps it
)

// CleanupItem is something cleanup can delete.
type CleanupItem struct {
	Kind string `json:"kind"`
	// ID is the image ID, the container's server ID or the volume's name;
	// empty for the build cache.
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"` // bytes; -1 if unknown
	Created time.Time `json:"created,omitzero"`
	// Default items are ticked to start with.
	Default bool `json:"default"`
}

// CleanupPlan is what can be deleted on a host.
type CleanupPlan struct {
	Host  string        `json:"host"`
	Items []CleanupItem `json:"items"`
}

// CleanupRequest says what to delete.
type CleanupRequest struct {
	Images     []string `json:"images,omitempty"`
	Containers []string `json:"containers,omitempty"`
	Volumes    []string `json:"volumes,omitempty"`
	BuildCache bool     `json:"buildCache,omitempty"`
}

// CleanupResult is what cleanup did.
type CleanupResult struct {
	Freed   int64           `json:"freed"`
	Removed []CleanupItem   `json:"removed"`
	Failed  []CleanupFailed `json:"failed"`
}

// CleanupFailed is an item that couldn't be deleted.
type CleanupFailed struct {
	CleanupItem
	Error string `json:"error"`
}

// leftover matches the name a recreated container's old one gets while the
// new one is made (manager, stack and self-update all use it).
var leftover = regexp.MustCompile(`^(.+)-old-\d{9,}$`)

// helperLabels mark containers hlm runs for a moment and removes.
var helperLabels = []string{"io.hlm.cleanup", "io.hlm.selfupdate-helper"}

// CleanupCandidates works out what can be deleted on a host.
func (m *Manager) CleanupCandidates(ctx context.Context, host string) (CleanupPlan, error) {
	c, err := m.docker.client(host)
	if err != nil {
		return CleanupPlan{}, err
	}
	plan := CleanupPlan{Host: host, Items: []CleanupItem{}}
	containers, err := m.docker.ListContainersWithSizeOn(ctx, host)
	if err != nil {
		return plan, fmt.Errorf("list containers: %w", err)
	}

	// Images no container uses.
	used := map[string]bool{}
	for _, ct := range containers {
		used[ct.ImageID] = true
	}
	images, err := c.ListImages(ctx)
	if err != nil {
		return plan, fmt.Errorf("list images: %w", err)
	}
	for _, img := range images {
		if used[img.ID] {
			continue
		}
		it := CleanupItem{ID: img.ID, Size: img.Size, Created: time.Unix(img.Created, 0)}
		if tags := img.Tags(); len(tags) > 0 {
			it.Kind, it.Name = CleanUnusedImage, strings.Join(tags, ", ")
		} else {
			it.Kind, it.Default = CleanOldImage, true
			it.Name = docker.ShortID(img.ID)
			if len(img.RepoDigests) > 0 {
				repo, _, _ := strings.Cut(img.RepoDigests[0], "@")
				it.Name = repo + " (" + docker.ShortID(img.ID) + ")"
			}
		}
		plan.Items = append(plan.Items, it)
	}

	// Stopped containers hlm left behind.
	names := map[string]bool{}
	for _, ct := range containers {
		names[strings.TrimPrefix(firstOr(ct.Names, ""), "/")] = true
	}
	for _, ct := range containers {
		if ct.State == "running" || ct.State == "restarting" || ct.State == "paused" {
			continue
		}
		name := strings.TrimPrefix(firstOr(ct.Names, ""), "/")
		helper := false
		for _, l := range helperLabels {
			if ct.Labels[l] == "true" {
				helper = true
			}
		}
		_, bare := SplitID(name)
		if strings.HasSuffix(bare, "-cleanup") && strings.HasPrefix(bare, "hlm-") {
			helper = true
		}
		if mt := leftover.FindStringSubmatch(bare); mt != nil && names[JoinID(host, mt[1])] && ct.Labels[deploy.LabelManaged] == "true" {
			// The server was recreated (its own name is back), so this is
			// the old container that should have been removed.
			helper = true
		}
		if !helper {
			continue
		}
		plan.Items = append(plan.Items, CleanupItem{Kind: CleanHelper, ID: ct.ID, Name: name, Size: ct.SizeRw,
			Created: time.Unix(ct.Created, 0), Default: true})
	}

	du, err := m.docker.SystemDiskUsage(ctx, host)
	if err != nil {
		return plan, fmt.Errorf("Docker's disk usage: %w", err)
	}
	if du.BuildCache > 0 {
		plan.Items = append(plan.Items, CleanupItem{Kind: CleanBuildCache, Name: "Build cache", Size: du.BuildCache})
	}

	// Volumes no container uses, except deleted servers' data.
	vols, err := c.ListVolumes(ctx, true)
	if err != nil {
		return plan, fmt.Errorf("list volumes: %w", err)
	}
	kept := m.keptVolumes(host)
	for _, v := range vols {
		if kept[v.Name] {
			continue
		}
		size, ok := du.VolumeSizes[v.Name]
		if !ok {
			size = -1
		}
		it := CleanupItem{Kind: CleanVolume, ID: v.Name, Name: v.Name, Size: size}
		if t, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
			it.Created = t
		}
		plan.Items = append(plan.Items, it)
	}

	order := map[string]int{CleanOldImage: 0, CleanHelper: 1, CleanUnusedImage: 2, CleanBuildCache: 3, CleanVolume: 4}
	sort.SliceStable(plan.Items, func(i, j int) bool {
		a, b := plan.Items[i], plan.Items[j]
		if a.Kind != b.Kind {
			return order[a.Kind] < order[b.Kind]
		}
		return a.Size > b.Size
	})
	return plan, nil
}

// keptVolumes are the volumes deleted servers' records keep, on a host.
func (m *Manager) keptVolumes(host string) map[string]bool {
	out := map[string]bool{}
	if host != LocalHost {
		return out // deleted servers' data is only tracked on the local host
	}
	list, err := m.DeletedServers()
	if err != nil {
		return out
	}
	for _, d := range list {
		for _, f := range d.Folders {
			if f.Volume {
				out[f.Path] = true
			}
		}
	}
	return out
}

// Cleanup deletes what req asks for on a host, as far as it's still a
// candidate (anything else is left alone, and reported as failed).
func (m *Manager) Cleanup(ctx context.Context, host string, req CleanupRequest) (CleanupResult, error) {
	plan, err := m.CleanupCandidates(ctx, host)
	if err != nil {
		return CleanupResult{}, err
	}
	c, err := m.docker.client(host)
	if err != nil {
		return CleanupResult{}, err
	}
	res := CleanupResult{Removed: []CleanupItem{}, Failed: []CleanupFailed{}}
	byKey := map[string]CleanupItem{}
	for _, it := range plan.Items {
		byKey[it.Kind+" "+it.ID] = it
	}
	find := func(id string, kinds ...string) (CleanupItem, bool) {
		for _, k := range kinds {
			if it, ok := byKey[k+" "+id]; ok {
				return it, true
			}
		}
		return CleanupItem{}, false
	}
	done := func(it CleanupItem, err error) {
		if err != nil {
			msg := err.Error()
			if docker.IsConflict(err) {
				msg = "in use: " + msg
			}
			res.Failed = append(res.Failed, CleanupFailed{CleanupItem: it, Error: msg})
			return
		}
		res.Removed = append(res.Removed, it)
		if it.Size > 0 {
			res.Freed += it.Size
		}
	}
	notCandidate := errors.New("not something cleanup removes (any more)")

	for _, id := range req.Containers {
		it, ok := find(id, CleanHelper)
		if !ok {
			done(CleanupItem{Kind: CleanHelper, ID: id, Name: id}, notCandidate)
			continue
		}
		done(it, m.docker.RemoveContainer(ctx, id, false))
	}
	// Containers first: an old container's image is free once it's gone.
	for _, id := range req.Images {
		it, ok := find(id, CleanOldImage, CleanUnusedImage)
		if !ok {
			done(CleanupItem{Kind: CleanOldImage, ID: id, Name: docker.ShortID(id)}, notCandidate)
			continue
		}
		done(it, c.RemoveImage(ctx, id))
	}
	for _, name := range req.Volumes {
		it, ok := find(name, CleanVolume)
		if !ok {
			done(CleanupItem{Kind: CleanVolume, ID: name, Name: name}, notCandidate)
			continue
		}
		done(it, c.RemoveVolume(ctx, name))
	}
	if req.BuildCache {
		it, _ := find("", CleanBuildCache)
		it.Kind, it.Name = CleanBuildCache, "Build cache"
		freed, err := c.PruneBuildCache(ctx)
		it.Size = freed
		done(it, err)
	}
	m.log.Info("cleaned up", "host", host, "removed", len(res.Removed), "failed", len(res.Failed), "freed", res.Freed)
	if host == LocalHost {
		m.RefreshStorage()
	}
	return res, nil
}

// CleanupLeftovers deletes a host's old images and hlm's leftover
// containers: the default items, for the weekly cleanup.
func (m *Manager) CleanupLeftovers(ctx context.Context, host string) (CleanupResult, error) {
	plan, err := m.CleanupCandidates(ctx, host)
	if err != nil {
		return CleanupResult{}, err
	}
	var req CleanupRequest
	for _, it := range plan.Items {
		switch {
		case !it.Default:
		case it.Kind == CleanHelper:
			req.Containers = append(req.Containers, it.ID)
		case it.Kind == CleanOldImage:
			req.Images = append(req.Images, it.ID)
		}
	}
	if len(req.Containers)+len(req.Images) == 0 {
		return CleanupResult{Removed: []CleanupItem{}, Failed: []CleanupFailed{}}, nil
	}
	return m.Cleanup(ctx, host, req)
}

// UseOldImageRemoval says whether to delete a server's old image after an
// update (or a change of image) succeeds; see removeOldImage.
func (m *Manager) UseOldImageRemoval(enabled func() bool) { m.removeOldImages = enabled }

// removeOldImage deletes the image a recreated server used before, if
// that's turned on, nothing else uses it and it has no tags left (an image
// still tagged is something the user may want). It's never forced.
func (m *Manager) removeOldImage(ctx context.Context, host, imageID, newImageID string, emit func(Event)) {
	if m.removeOldImages == nil || !m.removeOldImages() || imageID == "" || imageID == newImageID {
		return
	}
	c, err := m.docker.client(host)
	if err != nil {
		return
	}
	img, err := c.InspectImage(ctx, imageID)
	if err != nil || len((docker.ImageSummary{RepoTags: img.RepoTags}).Tags()) > 0 {
		return
	}
	list, err := c.ListContainers(ctx)
	if err != nil {
		return
	}
	for _, ct := range list {
		if ct.ImageID == imageID {
			return
		}
	}
	if err := c.RemoveImage(ctx, imageID); err != nil {
		m.log.Debug("remove old image", "image", imageID, "err", err)
		return
	}
	m.log.Info("removed old image", "host", host, "image", docker.ShortID(imageID))
	emit(Event{Stage: "create", Message: "Removed the old image (" + docker.ShortID(imageID) + ")"})
}
