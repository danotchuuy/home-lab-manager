package mods

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Change is a change someone asked for.
type Change struct {
	// Op is add, update, remove, disable or enable.
	Op string `json:"op"`
	// Project is the mod to add (its Modrinth ID or slug).
	Project string `json:"project,omitempty"`
	// Version is the version to add or update to; the newest that fits
	// if empty.
	Version string `json:"version,omitempty"`
	// File is the installed file to update, remove, disable or enable.
	File string `json:"file,omitempty"`
	// Why is set on dependencies the plan added.
	Why string `json:"why,omitempty"`
}

// Ops.
const (
	OpAdd     = "add"
	OpUpdate  = "update"
	OpRemove  = "remove"
	OpDisable = "disable"
	OpEnable  = "enable"
	// OpReplace is the step an update makes: one file for another.
	OpReplace = "replace"
)

// Step is one thing a plan does to the folder.
type Step struct {
	// Op is add, replace, remove, disable or enable.
	Op string `json:"op"`
	// File is the file in the folder now (replace, remove, disable,
	// enable).
	File string `json:"file,omitempty"`
	// Title is the mod's name, where known.
	Title string `json:"title,omitempty"`
	// New is the file added (add, replace).
	New *Download `json:"new,omitempty"`
	// FromVersion is the version replaced.
	FromVersion string `json:"fromVersion,omitempty"`
	// Why says why it's there (another mod needs it).
	Why string `json:"why,omitempty"`
}

// Download is a file to install.
type Download struct {
	Project       string `json:"project"`
	Version       string `json:"version"`
	VersionNumber string `json:"versionNumber"`
	File          string `json:"file"`
	Size          int64  `json:"size"`
	SHA512        string `json:"sha512"`
	vfile         VFile
}

// Plan is what a change would do.
type Plan struct {
	Steps []Step `json:"steps"`
	// Changes are the changes again, with the versions chosen and the
	// dependencies added: what to apply.
	Changes  []Change `json:"changes"`
	Warnings []string `json:"warnings,omitempty"`
	// Running says the server will be restarted to load them.
	Running bool `json:"running"`
}

// Limits on a plan.
const (
	maxChanges = 50
	maxSteps   = 80
)

// fileRE is a file name hlm writes into the mods folder.
var fileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+()\[\]!' -]{0,150}\.jar$`)

func checkFile(name string) error {
	if !fileRE.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%q isn't a file name hlm installs (a .jar without unusual characters)", name)
	}
	return nil
}

// fits says whether a version loads on the server.
func fits(v Version, fit Fit) bool {
	if !slices.ContainsFunc(v.Loaders, func(l string) bool { return slices.Contains(fit.Loaders, l) }) {
		return false
	}
	return fit.GameVersion == "" || slices.Contains(v.GameVersions, fit.GameVersion)
}

func describeFit(st *state) string {
	return fmt.Sprintf("%s on %s %s", strings.ToLower(st.ov.Loader), st.profile.Name, st.ov.Fit.GameVersion)
}

// project looks a project up, through the cache.
func (s *Service) project(ctx context.Context, id string) (Project, error) {
	s.mu.Lock()
	for _, c := range s.projects {
		if c.p.ID == id || c.p.Slug == id {
			s.mu.Unlock()
			return c.p, nil
		}
	}
	s.mu.Unlock()
	ps, err := s.opts.Modrinth.Projects(ctx, []string{id})
	if err != nil {
		return Project{}, err
	}
	if len(ps) == 0 {
		return Project{}, fmt.Errorf("Modrinth has no project %q", id)
	}
	s.mu.Lock()
	s.projects[ps[0].ID] = cachedProject{p: ps[0], fetched: s.opts.Now()}
	s.mu.Unlock()
	return ps[0], nil
}

// pick chooses a project's version: the one asked for, or the best that
// fits.
func (s *Service) pick(ctx context.Context, st *state, p Project, version string) (Version, error) {
	if version != "" {
		v, err := s.opts.Modrinth.Version(ctx, version)
		if err != nil {
			return Version{}, fmt.Errorf("%s version %s: %w", p.Title, version, err)
		}
		if v.ProjectID != p.ID {
			return Version{}, fmt.Errorf("version %s isn't a version of %s", version, p.Title)
		}
		if !fits(v, st.ov.Fit) {
			return Version{}, fmt.Errorf("%s %s doesn't run on this server (%s)", p.Title, v.VersionNumber, describeFit(st))
		}
		return v, nil
	}
	vs, err := s.opts.Modrinth.Versions(ctx, p.ID, st.ov.Fit)
	if err != nil {
		return Version{}, fmt.Errorf("%s: %w", p.Title, err)
	}
	vs = slices.DeleteFunc(vs, func(v Version) bool { return !fits(v, st.ov.Fit) })
	v, ok := Best(vs)
	if !ok {
		return Version{}, fmt.Errorf("%s has no version for this server (%s)", p.Title, describeFit(st))
	}
	return v, nil
}

// plan works out what changes do, with the dependencies they need.
func (s *Service) plan(ctx context.Context, st *state, changes []Change) (*Plan, error) {
	if st.ov.FitError != "" {
		return nil, errors.New(st.ov.FitError)
	}
	if len(changes) == 0 {
		return nil, errors.New("nothing to change")
	}
	if len(changes) > maxChanges {
		return nil, fmt.Errorf("at most %d changes at a time", maxChanges)
	}
	p := &Plan{Steps: []Step{}, Changes: []Change{}, Running: st.ov.Running}
	installed := map[string]*Installed{} // by project
	inFolder := map[string]*Installed{}  // by file
	for i := range st.ov.Mods {
		m := &st.ov.Mods[i]
		inFolder[m.File] = m
		if m.Project != "" {
			installed[m.Project] = m
		}
	}
	planned := map[string]bool{} // projects added or updated
	touched := map[string]bool{} // files changed, and files added
	type need struct {
		dep Dependency
		by  string
	}
	var deps []need

	touch := func(file string) error {
		base := strings.TrimSuffix(file, disabledSuffix)
		if touched[base] {
			return fmt.Errorf("%s is changed twice", base)
		}
		touched[base] = true
		return nil
	}
	install := func(v Version, title, why string, replace *Installed) error {
		f, ok := v.File()
		if !ok {
			return fmt.Errorf("%s %s has no file", title, v.VersionNumber)
		}
		if err := checkFile(f.Filename); err != nil {
			return fmt.Errorf("%s: %w", title, err)
		}
		if err := s.opts.Modrinth.checkURL(f.URL); err != nil {
			return fmt.Errorf("%s: %w", title, err)
		}
		if f.Size > MaxFileSize {
			return fmt.Errorf("%s is larger than %d MiB", f.Filename, MaxFileSize>>20)
		}
		replacing := replace != nil && strings.TrimSuffix(replace.File, disabledSuffix) == f.Filename
		if !replacing {
			for _, name := range []string{f.Filename, f.Filename + disabledSuffix} {
				if inFolder[name] != nil {
					return fmt.Errorf("a file called %s is already in the folder", name)
				}
			}
			if err := touch(f.Filename); err != nil {
				return err
			}
		}
		d := &Download{Project: v.ProjectID, Version: v.ID, VersionNumber: v.VersionNumber, File: f.Filename,
			Size: f.Size, SHA512: strings.ToLower(f.Hashes["sha512"]), vfile: f}
		step := Step{Op: OpAdd, Title: title, New: d, Why: why}
		change := Change{Op: OpAdd, Project: v.ProjectID, Version: v.ID, Why: why}
		if replace != nil {
			step.Op, step.File, step.FromVersion = OpReplace, replace.File, replace.VersionNumber
			change = Change{Op: OpUpdate, File: replace.File, Version: v.ID}
		}
		planned[v.ProjectID] = true
		p.Steps = append(p.Steps, step)
		p.Changes = append(p.Changes, change)
		for _, d := range v.Dependencies {
			switch d.Type {
			case "required":
				deps = append(deps, need{d, title})
			case "incompatible":
				if m := installed[d.ProjectID]; m != nil && d.ProjectID != "" {
					p.Warnings = append(p.Warnings, fmt.Sprintf("%s says it doesn't work with %s, which is installed.", title, cmpOr(m.Title, m.File)))
				}
			}
		}
		return nil
	}

	for _, c := range changes {
		switch c.Op {
		case OpAdd:
			if c.Project == "" {
				return nil, errors.New("add: which mod?")
			}
			pr, err := s.project(ctx, c.Project)
			if err != nil {
				return nil, err
			}
			if pr.ServerSide == "unsupported" {
				return nil, fmt.Errorf("%s only runs in the game itself, not on servers", pr.Title)
			}
			if m := installed[pr.ID]; m != nil {
				return nil, fmt.Errorf("%s is already installed (%s); update it instead", pr.Title, m.File)
			}
			if planned[pr.ID] {
				continue // another change already needs it
			}
			v, err := s.pick(ctx, st, pr, c.Version)
			if err != nil {
				return nil, err
			}
			if err := install(v, pr.Title, c.Why, nil); err != nil {
				return nil, err
			}
		case OpUpdate, OpRemove, OpDisable, OpEnable:
			m := inFolder[c.File]
			if m == nil {
				return nil, fmt.Errorf("%s isn't in the folder", c.File)
			}
			title := cmpOr(m.Title, m.File)
			switch c.Op {
			case OpUpdate:
				if m.Project == "" {
					return nil, fmt.Errorf("hlm doesn't know which mod %s is, so it can't update it", m.File)
				}
				pr, err := s.project(ctx, m.Project)
				if err != nil {
					return nil, err
				}
				v, err := s.pick(ctx, st, pr, c.Version)
				if err != nil {
					return nil, err
				}
				if v.ID == m.Version {
					return nil, fmt.Errorf("%s is already at %s, the newest version for this server", title, v.VersionNumber)
				}
				if err := touch(m.File); err != nil {
					return nil, err
				}
				if err := install(v, pr.Title, "", m); err != nil {
					return nil, err
				}
				continue
			case OpDisable:
				if m.Disabled {
					return nil, fmt.Errorf("%s is already off", title)
				}
			case OpEnable:
				if !m.Disabled {
					return nil, fmt.Errorf("%s is already on", title)
				}
				if inFolder[strings.TrimSuffix(m.File, disabledSuffix)] != nil {
					return nil, fmt.Errorf("%s is in the folder twice, turned on and off; remove one", strings.TrimSuffix(m.File, disabledSuffix))
				}
			}
			if err := touch(m.File); err != nil {
				return nil, err
			}
			p.Steps = append(p.Steps, Step{Op: c.Op, File: m.File, Title: m.Title})
			p.Changes = append(p.Changes, Change{Op: c.Op, File: m.File})
		default:
			return nil, fmt.Errorf("unknown change %q", c.Op)
		}
	}

	// The dependencies, and theirs.
	for len(deps) > 0 {
		n := deps[0]
		deps = deps[1:]
		if len(p.Steps) >= maxSteps {
			return nil, fmt.Errorf("that needs more than %d changes", maxSteps)
		}
		pid := n.dep.ProjectID
		if pid == "" && n.dep.VersionID != "" {
			v, err := s.opts.Modrinth.Version(ctx, n.dep.VersionID)
			if err != nil {
				return nil, fmt.Errorf("%s needs version %s of another mod: %w", n.by, n.dep.VersionID, err)
			}
			pid = v.ProjectID
		}
		if pid == "" {
			continue // a file name only: nothing hlm can install
		}
		if m := installed[pid]; m != nil {
			if m.Disabled && !touched[strings.TrimSuffix(m.File, disabledSuffix)] {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s needs %s, which is turned off.", n.by, cmpOr(m.Title, m.File)))
			}
			continue
		}
		if planned[pid] {
			continue
		}
		pr, err := s.project(ctx, pid)
		if err != nil {
			return nil, fmt.Errorf("%s needs another mod: %w", n.by, err)
		}
		v, err := s.pick(ctx, st, pr, n.dep.VersionID)
		if err != nil {
			return nil, fmt.Errorf("%s needs %s: %w", n.by, pr.Title, err)
		}
		if err := install(v, pr.Title, "needed by "+n.by, nil); err != nil {
			return nil, err
		}
	}
	return p, nil
}
