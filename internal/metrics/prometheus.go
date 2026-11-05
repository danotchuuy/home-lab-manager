package metrics

import (
	"io"
	"math"
	"strconv"
	"strings"
)

// Exposition collects metrics and writes them in Prometheus' text format
// (version 0.0.4): each family's HELP and TYPE once, then its samples.
type Exposition struct {
	order    []string
	families map[string]*family
}

type family struct {
	help, kind string
	lines      []string
}

// ContentType is the text format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Gauge adds a sample of a gauge. labels are name, value pairs.
func (e *Exposition) Gauge(name, help string, v float64, labels ...string) {
	e.add(name, "gauge", help, v, labels)
}

// Counter adds a sample of a counter.
func (e *Exposition) Counter(name, help string, v float64, labels ...string) {
	e.add(name, "counter", help, v, labels)
}

func (e *Exposition) add(name, kind, help string, v float64, labels []string) {
	if e.families == nil {
		e.families = map[string]*family{}
	}
	f := e.families[name]
	if f == nil {
		f = &family{help: help, kind: kind}
		e.families[name] = f
		e.order = append(e.order, name)
	}
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 1 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatValue(v))
	f.lines = append(f.lines, b.String())
}

// WriteTo writes the metrics.
func (e *Exposition) WriteTo(w io.Writer) (int64, error) {
	var b strings.Builder
	for _, name := range e.order {
		f := e.families[name]
		b.WriteString("# HELP " + name + " " + escapeHelp(f.help) + "\n")
		b.WriteString("# TYPE " + name + " " + f.kind + "\n")
		for _, l := range f.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeHelp(s string) string { return helpEscaper.Replace(s) }
