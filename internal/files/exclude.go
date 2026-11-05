package files

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Exclude patterns leave files out of an archive:
//
//   - a pattern without a slash matches a file or folder name anywhere
//     (*.log, steamapps);
//   - one ending in a slash only matches folders (cache/);
//   - one starting with a slash matches a whole container path
//     (/data/logs, /data/*/cache);
//   - another with a slash matches the end of a path (steamapps/common).
//
// Patterns use path.Match's syntax (*, ?, [a-z]). An excluded folder leaves
// out everything in it.

// MaxExclude is how many patterns a plan can have.
const MaxExclude = 50

// CheckExclude tidies and validates exclude patterns.
func CheckExclude(patterns []string) ([]string, error) {
	var out []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.ContainsRune(p, 0) || len(p) > 256 {
			return nil, fmt.Errorf("%q isn't a pattern", p)
		}
		if _, err := path.Match(strings.TrimSuffix(p, "/"), ""); err != nil {
			return nil, fmt.Errorf("%q isn't a valid pattern: %w", p, err)
		}
		if strings.Trim(p, "/") == "" {
			return nil, errors.New("a pattern of only slashes would leave everything out")
		}
		out = append(out, p)
	}
	if len(out) > MaxExclude {
		return nil, fmt.Errorf("at most %d patterns", MaxExclude)
	}
	return out, nil
}

// Excluded reports whether the container path full (a folder if dir) is
// left out by patterns.
func Excluded(patterns []string, full string, dir bool) bool {
	full = path.Clean(full)
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			if !dir {
				continue
			}
			p = strings.TrimSuffix(p, "/")
		}
		switch {
		case strings.HasPrefix(p, "/"):
			if ok, _ := path.Match(path.Clean(p), full); ok {
				return true
			}
		case strings.Contains(p, "/"):
			parts := strings.Split(strings.TrimPrefix(full, "/"), "/")
			for i := range parts {
				if ok, _ := path.Match(p, strings.Join(parts[i:], "/")); ok {
					return true
				}
			}
		default:
			if ok, _ := path.Match(p, path.Base(full)); ok {
				return true
			}
		}
	}
	return false
}
