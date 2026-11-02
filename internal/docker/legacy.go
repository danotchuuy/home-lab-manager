package docker

import "strings"

// Home Lab Manager's short name was gsh before it became hlm. Containers and
// networks made before then carry io.gsh.* labels; they're read as the
// io.hlm.* labels that replaced them, and listing by an io.hlm.* label finds
// them too. They get the new labels when they're next recreated.
const (
	labelPrefix       = "io.hlm."
	legacyLabelPrefix = "io.gsh."
)

// addCurrentLabels gives each io.gsh.* label its io.hlm.* name too, unless
// that's set already.
func addCurrentLabels(labels map[string]string) {
	var add []string
	for k := range labels {
		if rest, ok := strings.CutPrefix(k, legacyLabelPrefix); ok {
			if _, has := labels[labelPrefix+rest]; !has {
				add = append(add, rest)
			}
		}
	}
	for _, rest := range add {
		labels[labelPrefix+rest] = labels[legacyLabelPrefix+rest]
	}
}

// legacyFilters returns label filters with io.hlm.* labels by their old
// names, and whether there were any.
func legacyFilters(labels []string) ([]string, bool) {
	out := make([]string, len(labels))
	changed := false
	for i, l := range labels {
		if rest, ok := strings.CutPrefix(l, labelPrefix); ok {
			l, changed = legacyLabelPrefix+rest, true
		}
		out[i] = l
	}
	return out, changed
}
