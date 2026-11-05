package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ImageSummary is an image as the image list endpoint reports it.
type ImageSummary struct {
	ID          string            `json:"Id"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	Labels      map[string]string `json:"Labels"`
}

// Tags are the image's tags, without Docker's "<none>:<none>" for an
// untagged image.
func (i ImageSummary) Tags() []string {
	var out []string
	for _, t := range i.RepoTags {
		if t != "" && t != "<none>:<none>" {
			out = append(out, t)
		}
	}
	return out
}

// ListImages lists the top-level images (not intermediate layers).
func (c *Client) ListImages(ctx context.Context) ([]ImageSummary, error) {
	var out []ImageSummary
	err := c.doJSON(ctx, http.MethodGet, "/images/json", nil, nil, &out)
	return out, err
}

// RemoveImage deletes an image by ID, without force: an image a container
// uses, or that has several tags, is refused (status 409).
func (c *Client) RemoveImage(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/images/"+escapeRef(id), url.Values{"force": {"false"}}, nil, nil)
}

// Volume is a Docker volume.
type Volume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
}

// ListVolumes lists volumes; with dangling, only those no container uses.
func (c *Client) ListVolumes(ctx context.Context, dangling bool) ([]Volume, error) {
	q := url.Values{}
	if dangling {
		f, _ := json.Marshal(map[string][]string{"dangling": {"true"}})
		q.Set("filters", string(f))
	}
	var out struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/volumes", q, nil, &out)
	return out.Volumes, err
}

// PruneBuildCache deletes the build cache no build is using, and returns
// the space it freed.
func (c *Client) PruneBuildCache(ctx context.Context) (int64, error) {
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/build/prune", url.Values{"all": {"true"}}, nil, &out)
	return out.SpaceReclaimed, err
}

// IsConflict reports whether err is a 409 from the daemon, such as for an
// image a container uses.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict
}

// ShortID is an image or container ID as docker prints it.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
