package profileshare

import "strings"

// Line is one line of a difference: " " the same, "-" only in the old
// text, "+" only in the new.
type Line struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// maxDiffCells bounds the work a difference takes; profiles are small.
const maxDiffCells = 4_000_000

// Diff compares two texts line by line (longest common subsequence).
func Diff(old, new string) []Line {
	a, b := splitLines(old), splitLines(new)
	out := []Line{}
	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			out = append(out, Line{"-", l})
		}
		for _, l := range b {
			out = append(out, Line{"+", l})
		}
		return out
	}
	// lcs[i][j] is the common length of a[i:] and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, Line{" ", a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, Line{"-", a[i]})
			i++
		default:
			out = append(out, Line{"+", b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, Line{"-", a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, Line{"+", b[j]})
	}
	return out
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Changed says whether a difference changes anything.
func Changed(d []Line) bool {
	for _, l := range d {
		if l.Op != " " {
			return true
		}
	}
	return false
}
