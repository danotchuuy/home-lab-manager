package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PullProgress is one progress message from an image pull.
type PullProgress struct {
	ID             string `json:"id,omitempty"`
	Status         string `json:"status,omitempty"`
	Progress       string `json:"progress,omitempty"`
	ProgressDetail struct {
		Current int64 `json:"current,omitempty"`
		Total   int64 `json:"total,omitempty"`
	} `json:"progressDetail"`
	Error string `json:"error,omitempty"`
}

// PullImage pulls ref from its registry, calling progress (if non-nil) for
// each status message the daemon sends.
func (c *Client) PullImage(ctx context.Context, ref string, progress func(PullProgress)) error {
	name, tag := SplitImageRef(ref)
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	resp, err := c.doHeaders(ctx, http.MethodPost, "/images/create", q, nil, "", c.authHeader(ctx, ref))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	for {
		var p PullProgress
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if p.Error != "" {
			return errors.New(p.Error)
		}
		if progress != nil {
			progress(p)
		}
	}
}

// Image is the subset of image inspect output that we use.
type Image struct {
	ID string `json:"Id"`
	// Architecture and Os are what the image runs on (amd64, linux).
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
	RepoTags     []string `json:"RepoTags"`
	RepoDigests  []string `json:"RepoDigests"`
	// Config holds the defaults the image gives its containers.
	Config struct {
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		Labels       map[string]string   `json:"Labels"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
	} `json:"Config"`
}

// InspectImage returns a local image by reference or ID.
func (c *Client) InspectImage(ctx context.Context, ref string) (Image, error) {
	var out Image
	// Image references contain slashes, which the daemon's route accepts
	// unescaped.
	err := c.doJSON(ctx, http.MethodGet, "/images/"+escapeRef(ref)+"/json", nil, nil, &out)
	return out, err
}

// RemoteDigest asks the daemon to look up ref in its registry and returns the
// digest of the manifest (or multi-platform index) the tag points to, using
// the registry login SetCredentials finds for it, if any.
func (c *Client) RemoteDigest(ctx context.Context, ref string) (string, error) {
	return c.remoteDigest(ctx, ref, c.authHeader(ctx, ref))
}

// RemoteDigestAs is RemoteDigest with the given login, to test it.
func (c *Client) RemoteDigestAs(ctx context.Context, ref string, a RegistryAuth) (string, error) {
	return c.remoteDigest(ctx, ref, http.Header{"X-Registry-Auth": {a.Encode()}})
}

func (c *Client) remoteDigest(ctx context.Context, ref string, header http.Header) (string, error) {
	var out struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	resp, err := c.doHeaders(ctx, http.MethodGet, "/distribution/"+escapeRef(ref)+"/json", nil, nil, "", header)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Descriptor.Digest == "" {
		return "", errors.New("registry returned no digest")
	}
	return out.Descriptor.Digest, nil
}

// escapeRef escapes an image reference for use in a URL path, keeping the
// slashes that separate its parts.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// SplitImageRef splits an image reference into its name and its tag or
// digest. A reference without a tag gets "latest".
func SplitImageRef(ref string) (name, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	// A colon after the last slash separates the tag. A colon before it is a
	// registry port, as in registry:5000/image.
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}
