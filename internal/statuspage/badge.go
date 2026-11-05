package statuspage

import (
	"fmt"
	"html"
	"strconv"
	"sync"
	"time"
)

// Badge is a shields-style SVG badge for a server: its name, then its state
// (with players, when shown), e.g. "Valheim | 3/10 online".
func Badge(srv Server) []byte {
	label := srv.Name
	value, color := "offline", "#9f9f9f"
	switch srv.State {
	case StateOnline:
		value, color = "online", "#2b8a3e"
		if srv.Players != nil {
			value = strconv.Itoa(*srv.Players)
			if srv.MaxPlayers != nil {
				value += "/" + strconv.Itoa(*srv.MaxPlayers)
			}
			value += " online"
		}
	case StateStarting:
		value, color = "starting", "#b07500"
	case StateRunning:
		value, color = "up", "#2b8a3e"
	}
	lw, vw := textWidth(label)+10, textWidth(value)+10
	w := lw + vw
	esc := html.EscapeString
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`+
		`<title>%s: %s</title>`+
		`<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>`+
		`<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`+
		`<g clip-path="url(#r)"><rect width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/><rect width="%d" height="20" fill="url(#s)"/></g>`+
		`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">`+
		`<text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text><text x="%d" y="14">%s</text>`+
		`<text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text><text x="%d" y="14">%s</text></g></svg>`,
		w, esc(label), esc(value), esc(label), esc(value),
		w, lw, lw, vw, color, w,
		lw/2, esc(label), lw/2, esc(label),
		lw+vw/2, esc(value), lw+vw/2, esc(value))
}

// textWidth estimates text's width in 11px Verdana, as badge makers do.
func textWidth(s string) int {
	w := 0.0
	for _, r := range s {
		switch {
		case r == ' ' || r == 'i' || r == 'l' || r == '|' || r == '.' || r == ',' || r == ':' || r == '!' || r == '\'':
			w += 3.5
		case r == 'm' || r == 'w' || r == 'M' || r == 'W':
			w += 10
		case r >= 'A' && r <= 'Z':
			w += 8
		case r >= '0' && r <= '9':
			w += 7
		default:
			w += 6.8
		}
	}
	return int(w + 0.5)
}

// Limiter lets each address make a burst of requests, refilled steadily.
type Limiter struct {
	Rate  float64 // requests per second
	Burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Allow reports whether a request from addr may go ahead now.
func (l *Limiter) Allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	if now.Sub(l.swept) > 10*time.Minute { // keep the map small
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b := l.buckets[addr]
	if b == nil {
		b = &bucket{tokens: l.Burst, last: now}
		l.buckets[addr] = b
	}
	b.tokens = min(l.Burst, b.tokens+now.Sub(b.last).Seconds()*l.Rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
