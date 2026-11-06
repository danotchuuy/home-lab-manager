package profileshare

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const indexesSecret = "game-profile-indexes"

// maxIndexes bounds the index links kept.
const maxIndexes = 10

// Options configures the service.
type Options struct {
	Store   *gameprofile.Store
	Secrets *secrets.Store
	Fetcher *Fetcher
	Log     *slog.Logger
	Now     func() time.Time
}

// Service imports and updates shared profiles.
type Service struct {
	opts Options
}

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{opts: o}
}

// Preview is a fetched profile, before it's saved.
type Preview struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	YAML   string `json:"yaml"`
	// Index is the index it's listed in, when it came from one.
	Index string `json:"index,omitempty"`
	// Replaces is what saving it takes the place of: a built-in profile,
	// your own ("custom"), or nothing ("": a new game).
	Replaces string `json:"replaces,omitempty"`
	// Diff is the difference from the profile it replaces.
	Diff []Line `json:"diff,omitempty"`
	// Same says it's exactly the profile already in use.
	Same bool `json:"same,omitempty"`
}

func (s *Service) preview(link string, data []byte, p *gameprofile.Profile) Preview {
	pv := Preview{URL: link, SHA256: gameprofile.Hash(data), ID: p.ID, Name: p.Name, YAML: string(data)}
	if cur := gameprofile.Current().ByID(p.ID); cur != nil {
		pv.Replaces = cur.Source
		old := string(cur.YAML())
		pv.Diff = Diff(old, string(data))
		pv.Same = !Changed(pv.Diff)
	}
	return pv
}

// Preview fetches a profile from a link, without saving it.
func (s *Service) Preview(ctx context.Context, link string) (Preview, error) {
	link, err := Normalize(link)
	if err != nil {
		return Preview{}, err
	}
	data, p, err := s.opts.Fetcher.Profile(ctx, link)
	if err != nil {
		return Preview{}, err
	}
	return s.preview(link, data, p), nil
}

// ErrChanged is a link that serves something else than what was looked
// at.
var ErrChanged = errors.New("the profile at that link changed since you looked at it; look at it again")

// Import fetches a profile from a link and saves it, with where it came
// from. sha256 is what the person looked at: the link must still serve
// exactly that.
func (s *Service) Import(ctx context.Context, link, sha256 string) (*gameprofile.Profile, error) {
	link, err := Normalize(link)
	if err != nil {
		return nil, err
	}
	data, _, err := s.opts.Fetcher.Profile(ctx, link)
	if err != nil {
		return nil, err
	}
	hash := gameprofile.Hash(data)
	if hash != sha256 {
		return nil, ErrChanged
	}
	s.opts.Log.Info("importing game profile", "url", link)
	return s.opts.Store.ImportFrom(data, &gameprofile.Origin{URL: link, SHA256: hash, Fetched: s.opts.Now().UTC()})
}

// Check fetches each profile imported from a link (or an index) again and
// notes the ones that changed. Nothing is replaced.
func (s *Service) Check(ctx context.Context) error {
	linked, err := s.opts.Store.Linked()
	if err != nil {
		return err
	}
	indexes := map[string]*Index{}
	indexErr := map[string]error{}
	for _, l := range linked {
		var hash string
		var ferr error
		if l.Origin.Index != "" {
			// What the index says it is now.
			ix, ok := indexes[l.Origin.Index]
			if !ok && indexErr[l.Origin.Index] == nil {
				got, err := s.opts.Fetcher.Index(ctx, l.Origin.Index)
				if err != nil {
					indexErr[l.Origin.Index] = err
				} else {
					ix = &got
					indexes[l.Origin.Index] = ix
				}
			}
			switch {
			case indexErr[l.Origin.Index] != nil:
				ferr = indexErr[l.Origin.Index]
			default:
				i := slices.IndexFunc(ix.Profiles, func(e IndexEntry) bool { return e.ID == l.ID })
				if i < 0 {
					ferr = errors.New("the index doesn't list it any more")
				} else {
					hash = ix.Profiles[i].SHA256
				}
			}
		} else {
			data, _, err := s.opts.Fetcher.Profile(ctx, l.Origin.URL)
			if err != nil {
				ferr = err
			} else {
				hash = gameprofile.Hash(data)
			}
		}
		if err := s.opts.Store.SetChecked(l.ID, hash, ferr, s.opts.Now()); err != nil {
			return err
		}
	}
	return nil
}

// Run checks for updates once a day until ctx ends, starting after first.
func (s *Service) Run(ctx context.Context, first time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if err := s.Check(cctx); err != nil {
			s.opts.Log.Warn("game profile update check", "err", err)
		}
		cancel()
		t.Reset(24 * time.Hour)
	}
}

// Indexes are the index links.
func (s *Service) Indexes() ([]string, error) {
	var out []string
	_, err := s.opts.Secrets.Get(indexesSecret, &out)
	if out == nil {
		out = []string{}
	}
	return out, err
}

// SetIndexes replaces the index links.
func (s *Service) SetIndexes(links []string) ([]string, error) {
	if len(links) > maxIndexes {
		return nil, fmt.Errorf("at most %d indexes", maxIndexes)
	}
	out := []string{}
	for _, l := range links {
		n, err := Normalize(l)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, s.opts.Secrets.Set(indexesSecret, out)
}

// Listing is one index's profiles, as browsed.
type Listing struct {
	URL      string         `json:"url"`
	Name     string         `json:"name,omitempty"`
	Error    string         `json:"error,omitempty"`
	Profiles []ListingEntry `json:"profiles"`
}

// ListingEntry is a profile in an index, and whether it's in use.
type ListingEntry struct {
	IndexEntry
	// InUse is the source of the profile with its id in use: built-in,
	// custom, or "".
	InUse string `json:"inUse,omitempty"`
	// Installed says your own profile came from this entry; Current says
	// it's the version listed.
	Installed bool `json:"installed,omitempty"`
	Current   bool `json:"current,omitempty"`
}

// Browse lists every index's profiles.
func (s *Service) Browse(ctx context.Context) ([]Listing, error) {
	links, err := s.Indexes()
	if err != nil {
		return nil, err
	}
	linked, err := s.opts.Store.Linked()
	if err != nil {
		return nil, err
	}
	out := []Listing{}
	for _, link := range links {
		l := Listing{URL: link, Profiles: []ListingEntry{}}
		ix, err := s.opts.Fetcher.Index(ctx, link)
		if err != nil {
			l.Error = err.Error()
			out = append(out, l)
			continue
		}
		l.Name = ix.Name
		for _, e := range ix.Profiles {
			le := ListingEntry{IndexEntry: e}
			if p := gameprofile.Current().ByID(e.ID); p != nil {
				le.InUse = p.Source
			}
			for _, k := range linked {
				if k.ID == e.ID && k.Origin.Index == link {
					le.Installed, le.Current = true, k.Origin.SHA256 == e.SHA256
				}
			}
			l.Profiles = append(l.Profiles, le)
		}
		sort.SliceStable(l.Profiles, func(i, j int) bool { return l.Profiles[i].Name < l.Profiles[j].Name })
		out = append(out, l)
	}
	return out, nil
}

// fromIndex fetches one of an index's profiles, checking it's what the
// index lists.
func (s *Service) fromIndex(ctx context.Context, index, id string) ([]byte, *gameprofile.Profile, IndexEntry, error) {
	index, err := Normalize(index)
	if err != nil {
		return nil, nil, IndexEntry{}, err
	}
	links, err := s.Indexes()
	if err != nil {
		return nil, nil, IndexEntry{}, err
	}
	if !slices.Contains(links, index) {
		return nil, nil, IndexEntry{}, errors.New("that index isn't one of yours; add it first")
	}
	ix, err := s.opts.Fetcher.Index(ctx, index)
	if err != nil {
		return nil, nil, IndexEntry{}, err
	}
	i := slices.IndexFunc(ix.Profiles, func(e IndexEntry) bool { return e.ID == id })
	if i < 0 {
		return nil, nil, IndexEntry{}, fmt.Errorf("the index doesn't list %q", id)
	}
	e := ix.Profiles[i]
	data, p, err := s.opts.Fetcher.Profile(ctx, e.URL)
	if err != nil {
		return nil, nil, e, err
	}
	if gameprofile.Hash(data) != e.SHA256 {
		return nil, nil, e, fmt.Errorf("%s isn't the profile the index lists (its SHA-256 doesn't match), so hlm won't use it", e.URL)
	}
	if p.ID != e.ID {
		return nil, nil, e, fmt.Errorf("the index lists %s, but the file is %s", e.ID, p.ID)
	}
	return data, p, e, nil
}

// PreviewIndexed fetches one of an index's profiles, without saving it.
func (s *Service) PreviewIndexed(ctx context.Context, index, id string) (Preview, error) {
	data, p, e, err := s.fromIndex(ctx, index, id)
	if err != nil {
		return Preview{}, err
	}
	pv := s.preview(e.URL, data, p)
	pv.Index, _ = Normalize(index)
	return pv, nil
}

// ImportIndexed saves one of an index's profiles. sha256 is what the
// person looked at.
func (s *Service) ImportIndexed(ctx context.Context, index, id, sha256 string) (*gameprofile.Profile, error) {
	data, _, e, err := s.fromIndex(ctx, index, id)
	if err != nil {
		return nil, err
	}
	if e.SHA256 != sha256 {
		return nil, ErrChanged
	}
	index, _ = Normalize(index)
	s.opts.Log.Info("importing game profile", "index", index, "id", id)
	return s.opts.Store.ImportFrom(data, &gameprofile.Origin{URL: e.URL, SHA256: e.SHA256, Index: index, Fetched: s.opts.Now().UTC()})
}

// Export is your own profiles, as a .zip.
func (s *Service) Export() ([]byte, error) {
	files, err := s.opts.Store.Files()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: s.opts.Now()})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
