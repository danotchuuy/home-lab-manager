package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// nginx allows one stream { } block. If the relay already has one (in
// nginx.conf or a file it includes), hlm can't add its own, so instead it
// "adopts" the existing block: it adds an include of its file inside it and
// leaves the rest of the block alone.

const includeLine = "include " + ConfigPath + ";"

// confToken is a word or one of { } ; in an nginx configuration.
type confToken struct {
	text       string
	start, end int // byte offsets
}

// tokenize splits an nginx configuration into tokens, skipping comments
// and keeping quoted strings (quotes included) as single words.
func tokenize(src string) []confToken {
	var toks []confToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == ';':
			toks = append(toks, confToken{string(c), i, i + 1})
			i++
		default:
			start := i
			for i < len(src) {
				c := src[i]
				if c == '"' || c == '\'' {
					q := c
					i++
					for i < len(src) && src[i] != q {
						if src[i] == '\\' {
							i++
						}
						i++
					}
					i++
					continue
				}
				if c == '\\' {
					i += 2
					continue
				}
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '{' || c == '}' || c == ';' || c == '#' {
					break
				}
				i++
			}
			if i > len(src) {
				i = len(src)
			}
			toks = append(toks, confToken{src[start:i], start, i})
		}
	}
	return toks
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// block is a top-level stream { } block.
type block struct {
	start       int // offset of the "stream" keyword
	open, close int // offsets of its braces
	included    bool
}

// streamBlock finds a top-level stream { } block and whether it already
// includes hlm's file.
func streamBlock(src string) (block, bool) {
	toks := tokenize(src)
	depth := 0
	atStart := true // the next word starts a directive
	for i, t := range toks {
		switch t.text {
		case "{":
			depth++
			atStart = true
		case "}":
			depth--
			atStart = true
		case ";":
			atStart = true
		default:
			if depth == 0 && atStart && t.text == "stream" && i+1 < len(toks) && toks[i+1].text == "{" {
				// Find the matching brace, noting any include of hlm's file.
				d, included := 0, false
				for j := i + 1; j < len(toks); j++ {
					switch toks[j].text {
					case "{":
						d++
					case "}":
						d--
						if d == 0 {
							return block{t.start, toks[i+1].start, toks[j].start, included}, true
						}
					case "include":
						if j+1 < len(toks) && isConfigFile(unquote(toks[j+1].text)) {
							included = true
						}
					}
				}
				return block{}, false // unbalanced
			}
			atStart = false
		}
	}
	return block{}, false
}

// isConfigFile reports whether an include names hlm's file, ConfigPath or
// legacyConfigPath.
func isConfigFile(p string) bool {
	b := path.Base(p)
	return b == path.Base(ConfigPath) || b == legacyConfigFile
}

// includesHlm reports whether a configuration includes hlm's file anywhere.
func includesHlm(src string) bool {
	toks := tokenize(src)
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].text == "include" && isConfigFile(unquote(toks[i+1].text)) {
			return true
		}
	}
	return false
}

// dumpedFile is one file from "nginx -T".
type dumpedFile struct {
	path    string
	content string
}

// parseDump splits "nginx -T" output into the files it shows.
func parseDump(out string) []dumpedFile {
	const header = "# configuration file "
	var files []dumpedFile
	lines := strings.SplitAfter(out, "\n")
	var cur *dumpedFile
	for _, l := range lines {
		if strings.HasPrefix(l, header) && strings.HasSuffix(strings.TrimRight(l, "\r\n"), ":") {
			files = append(files, dumpedFile{path: strings.TrimSuffix(strings.TrimRight(strings.TrimPrefix(l, header), "\r\n"), ":")})
			cur = &files[len(files)-1]
			continue
		}
		if cur != nil {
			cur.content += l
		}
	}
	return files
}

// insertInclude adds hlm's include as the first line inside the block that
// opens at open, indented like the block's next line.
func insertInclude(src string, open int) string {
	indent := "    "
	rest := src[open+1:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		next := rest[nl+1:]
		if w := len(next) - len(strings.TrimLeft(next, " \t")); w > 0 && strings.TrimSpace(next) != "}" {
			indent = next[:w]
		}
	}
	add := "\n" + indent + "# Added by Home Lab Manager: relays for game servers.\n" + indent + includeLine
	return src[:open+1] + add + src[open+1:]
}

// Adoption describes adding hlm's include to an existing stream block.
type Adoption struct {
	// File holds the stream block.
	File string `json:"file"`
	// Block is the stream block as it is now, and Proposed after the change.
	Block    string `json:"block"`
	Proposed string `json:"proposed"`
	// Included means the block already includes hlm's file; nothing to do.
	Included bool `json:"included"`
	// Found is false when nginx has no stream block (hlm adds its own).
	Found   bool   `json:"found"`
	content string // the file now
	updated string // the file after the change
}

const dumpScript = `PATH="$PATH:/usr/sbin:/sbin"
nginx -T 2>&1`

// PlanAdoption finds the relay's stream block and works out the change.
func (s *Service) PlanAdoption(ctx context.Context) (Adoption, error) {
	res, err := s.run(ctx, dumpScript, nil, nil)
	if err != nil {
		return Adoption{}, err
	}
	if res.ExitCode != 0 {
		return Adoption{}, fmt.Errorf("nginx's configuration doesn't pass \"nginx -t\" right now, so it can't be changed safely:\n%s", res.Output())
	}
	files := parseDump(res.Stdout)
	for _, f := range files {
		if _, ok := streamBlock(f.content); !ok {
			continue
		}
		if !strings.HasPrefix(f.path, "/") || strings.ContainsAny(f.path, "\n\x00") {
			return Adoption{}, fmt.Errorf("unexpected configuration file name %q", f.path)
		}
		// Work on the file itself: nginx -T's copy may differ in the
		// trailing newlines.
		cat, err := s.run(ctx, "cat -- "+remote.Quote(f.path), nil, nil)
		if err != nil {
			return Adoption{}, err
		}
		if cat.ExitCode != 0 {
			return Adoption{}, fmt.Errorf("read %s: %s", f.path, cat.Output())
		}
		content := cat.Stdout
		b, ok := streamBlock(content)
		if !ok {
			return Adoption{}, fmt.Errorf("%s changed while it was being read; try again", f.path)
		}
		a := Adoption{File: f.path, Found: true, Included: b.included, content: content, Block: blockText(content, b)}
		if !b.included {
			a.updated = insertInclude(content, b.open)
			nb, _ := streamBlock(a.updated)
			a.Proposed = blockText(a.updated, nb)
		}
		return a, nil
	}
	return Adoption{}, nil
}

// blockText is the stream block, from the start of its line to its
// closing brace.
func blockText(src string, b block) string {
	return src[strings.LastIndexByte(src[:b.start], '\n')+1 : b.close+1]
}

// adoptScript replaces the file (whose path and expected SHA-256 come
// first) with stdin, keeping its owner and mode, then checks nginx and
// rolls back if the check fails.
const adoptScript = `PATH="$PATH:/usr/sbin:/sbin"
have=$(sha256sum "$f" | cut -d' ' -f1)
[ "$have" = "$want" ] || { echo "hlm:changed"; exit 3; }
cp -p "$f" "$f.hlm-bak" || exit 1
tmp=$(mktemp) || exit 1
cat > "$tmp"
cat "$tmp" > "$f"; rm -f "$tmp"
conf=` + ConfigPath + `
created=0
if [ ! -f "$conf" ]; then printf '# Managed by Home Lab Manager.\n' > "$conf"; chmod 644 "$conf"; created=1; fi
if ! out=$(nginx -t 2>&1); then
  echo "$out"
  cat "$f.hlm-bak" > "$f"
  [ $created = 1 ] && rm -f "$conf"
  echo "hlm:testfailed"
  exit 4
fi
echo "$out"
if [ -d /run/systemd/system ] && systemctl cat nginx >/dev/null 2>&1; then
  systemctl reload-or-restart nginx || exit 5
elif ! nginx -s reload 2>/dev/null; then
  nginx || exit 5
fi
echo "hlm:adopted"
`

// Adopt adds hlm's include to the relay's existing stream block, keeping a
// backup of the file (<file>.hlm-bak).
func (s *Service) Adopt(ctx context.Context) (Adoption, string, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	a, err := s.PlanAdoption(ctx)
	if err != nil {
		return a, "", err
	}
	if !a.Found {
		return a, "", errors.New("nginx has no stream block to adopt; applying adds hlm's own")
	}
	if a.Included {
		return a, "", nil
	}
	sum := sha256.Sum256([]byte(a.content))
	script := "f=" + remote.Quote(a.File) + "\nwant=" + hex.EncodeToString(sum[:]) + "\n" + adoptScript
	res, err := s.run(ctx, script, []byte(a.updated), nil)
	out := strings.TrimSpace(strings.NewReplacer("hlm:adopted", "", "hlm:testfailed", "", "hlm:changed", "").Replace(res.Output()))
	if err != nil {
		return a, out, err
	}
	switch {
	case strings.Contains(res.Stdout, "hlm:adopted"):
		s.log.Info("added hlm's include to the existing stream block", "file", a.File)
		a.Included, a.Block, a.Proposed = true, a.Proposed, ""
		return a, out, nil
	case strings.Contains(res.Stdout, "hlm:changed"):
		return a, out, fmt.Errorf("%s changed in the meantime; review the change again", a.File)
	case strings.Contains(res.Stdout, "hlm:testfailed"):
		return a, out, fmt.Errorf("nginx rejected the change, so %s was put back", a.File)
	default:
		return a, out, fmt.Errorf("changing %s failed (exit status %d)", a.File, res.ExitCode)
	}
}
