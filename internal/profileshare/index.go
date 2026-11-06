package profileshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Index is a list of profiles someone publishes, so they can be browsed
// and imported in hlm.
type Index struct {
	Name     string       `yaml:"name" json:"name,omitempty"`
	Profiles []IndexEntry `yaml:"profiles" json:"profiles"`
}

// IndexEntry is one profile in an index.
type IndexEntry struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	URL         string `yaml:"url" json:"url"`
	SHA256      string `yaml:"sha256" json:"sha256"`
	Description string `yaml:"description" json:"description,omitempty"`
}

var (
	hashRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	profileID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// maxIndexEntries bounds an index.
const maxIndexEntries = 500

// Index fetches and checks an index. Relative profile links are taken
// against the index's own link.
func (f *Fetcher) Index(ctx context.Context, link string) (Index, error) {
	data, err := f.get(ctx, link, MaxIndexSize)
	if err != nil {
		return Index{}, err
	}
	return ParseIndex(data, link)
}

// ParseIndex reads an index fetched from link.
func ParseIndex(data []byte, link string) (Index, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var ix Index
	if err := dec.Decode(&ix); err != nil {
		return Index{}, fmt.Errorf("that isn't a profile index: %v", err)
	}
	if len(ix.Profiles) == 0 {
		return Index{}, errors.New("the index lists no profiles")
	}
	if len(ix.Profiles) > maxIndexEntries {
		return Index{}, fmt.Errorf("the index lists more than %d profiles", maxIndexEntries)
	}
	base, err := Normalize(link)
	if err != nil {
		return Index{}, err
	}
	seen := map[string]bool{}
	for i := range ix.Profiles {
		e := &ix.Profiles[i]
		if !profileID.MatchString(e.ID) {
			return Index{}, fmt.Errorf("index entry %d: %q isn't a profile id", i+1, e.ID)
		}
		if seen[e.ID] {
			return Index{}, fmt.Errorf("the index lists %s twice", e.ID)
		}
		seen[e.ID] = true
		e.SHA256 = strings.ToLower(strings.TrimSpace(e.SHA256))
		if !hashRE.MatchString(e.SHA256) {
			return Index{}, fmt.Errorf("index entry %s: sha256 must be the profile's SHA-256, in hex", e.ID)
		}
		u, err := resolve(base, e.URL)
		if err != nil {
			return Index{}, fmt.Errorf("index entry %s: %w", e.ID, err)
		}
		e.URL = u
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" {
			e.Name = e.ID
		}
		if len(e.Description) > 500 {
			e.Description = e.Description[:500]
		}
	}
	return ix, nil
}
