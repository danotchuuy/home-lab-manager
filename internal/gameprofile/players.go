package gameprofile

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Players is how a game's console lists the players online.
type Players struct {
	// Command lists them.
	Command string `yaml:"command" json:"command"`
	// Format is "csv", "summary" or "lines".
	Format string `yaml:"format" json:"format"`
	// Header starts the first line of csv output (any case).
	Header string `yaml:"header" json:"header,omitempty"`
	// Pattern is summary's whole-output pattern (groups online, max and
	// names) or lines' per-line pattern (group name).
	Pattern string `yaml:"pattern" json:"pattern,omitempty"`
	// NamesSeparator splits summary's names; "," if empty.
	NamesSeparator string `yaml:"names_separator" json:"namesSeparator,omitempty"`
	// Ignore are lines to skip in lines output (any case), such as "No
	// Players Connected".
	Ignore []string `yaml:"ignore" json:"ignore,omitempty"`
	// IDColumn is csv's column (from 1) with each player's ID, for actions
	// that take it; lines' pattern can have an id group instead.
	IDColumn int `yaml:"id_column" json:"idColumn,omitempty"`

	re *regexp.Regexp
}

// PlayerList is what a player list says.
type PlayerList struct {
	Online int
	// Max is the player limit, if it says; 0 otherwise.
	Max   int
	Names []string
	// IDs are the players' IDs, beside Names, when the list has them.
	IDs []string
}

// HasIDs says whether the list gives players' IDs.
func (p *Players) HasIDs() bool {
	switch p.Format {
	case "csv":
		return p.IDColumn > 1
	case "lines":
		if p.re == nil {
			p.compile()
		}
		return p.re != nil && p.re.SubexpIndex("id") >= 0
	}
	return false
}

func (p *Players) compile() error {
	if strings.TrimSpace(p.Command) == "" {
		return errors.New("needs the command that lists them")
	}
	switch p.Format {
	case "csv":
		if p.Header == "" {
			return errors.New("csv needs the header its first line starts with")
		}
		if p.IDColumn == 1 || p.IDColumn < 0 || p.IDColumn > 50 {
			return errors.New("id_column is the column with the ID, from 2 (the name is the first)")
		}
		return nil
	case "summary", "lines":
		if p.IDColumn != 0 {
			return errors.New("id_column is for csv; give the pattern an id group instead")
		}
	default:
		return fmt.Errorf("unknown format %q (csv, summary or lines)", p.Format)
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return fmt.Errorf("pattern: %w", err)
	}
	need := []string{"name"}
	if p.Format == "summary" {
		need = []string{"online", "max", "names"}
	}
	for _, g := range need {
		if re.SubexpIndex(g) < 0 {
			return fmt.Errorf("pattern needs a (?P<%s>…) group", g)
		}
	}
	p.re = re
	return nil
}

// Parse reads the command's output.
func (p *Players) Parse(out string) (PlayerList, error) {
	if p.re == nil && p.Format != "csv" {
		if err := p.compile(); err != nil {
			return PlayerList{}, err
		}
	}
	var res PlayerList
	out = strings.ReplaceAll(out, "\r\n", "\n")
	switch p.Format {
	case "csv":
		// A header line, then one player per line: name first.
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) == 0 || !strings.HasPrefix(strings.ToLower(lines[0]), strings.ToLower(p.Header)) {
			return res, fmt.Errorf("unexpected %s output", p.Command)
		}
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			res.Online++
			cols := strings.Split(l, ",")
			if cols[0] != "" {
				res.Names = append(res.Names, cols[0])
				if p.IDColumn > 1 {
					id := ""
					if len(cols) >= p.IDColumn {
						id = strings.TrimSpace(cols[p.IDColumn-1])
					}
					res.IDs = append(res.IDs, id)
				}
			}
		}
	case "summary":
		m := p.re.FindStringSubmatch(out)
		if m == nil {
			return res, fmt.Errorf("unexpected %s output", p.Command)
		}
		res.Online, _ = strconv.Atoi(m[p.re.SubexpIndex("online")])
		res.Max, _ = strconv.Atoi(m[p.re.SubexpIndex("max")])
		sep := p.NamesSeparator
		if sep == "" {
			sep = ","
		}
		for _, n := range strings.Split(m[p.re.SubexpIndex("names")], sep) {
			if n = strings.TrimSpace(n); n != "" {
				res.Names = append(res.Names, n)
			}
		}
	case "lines":
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || isIgnored(l, p.Ignore) {
				continue
			}
			if m := p.re.FindStringSubmatch(l); m != nil {
				res.Online++
				res.Names = append(res.Names, strings.TrimSpace(m[p.re.SubexpIndex("name")]))
				if i := p.re.SubexpIndex("id"); i >= 0 {
					res.IDs = append(res.IDs, strings.TrimSpace(m[i]))
				}
			}
		}
	}
	return res, nil
}

func isIgnored(l string, ignore []string) bool {
	for _, i := range ignore {
		if strings.EqualFold(l, i) {
			return true
		}
	}
	return false
}
