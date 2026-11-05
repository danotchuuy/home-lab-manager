package agent

import (
	"regexp"
	"strings"
)

// The Docker API calls the agent passes on: those hlm makes, and nothing
// else (no swarm, plugins, secrets, image builds or exports, container
// commits…). hlm only sends these anyway; the agent checks again, so a
// compromised or impersonated hlm can't use the rest of the API.

type rule struct {
	methods string // space-separated
	path    *regexp.Regexp
}

const (
	id  = `[A-Za-z0-9][A-Za-z0-9_.-]*`    // a container, network, volume or exec ID or name
	ref = `[A-Za-z0-9][A-Za-z0-9_.:@/-]*` // an image reference
)

func r(methods, pattern string) rule {
	return rule{methods: " " + methods + " ", path: regexp.MustCompile("^" + pattern + "$")}
}

var rules = []rule{
	r("GET HEAD", `/_ping`),
	r("GET", `/(version|info|events|system/df)`),

	r("GET", `/containers/json`),
	r("POST", `/containers/create`),
	r("GET", `/containers/`+id+`/(json|logs|stats|top)`),
	r("GET HEAD PUT", `/containers/`+id+`/archive`),
	r("POST", `/containers/`+id+`/(start|stop|restart|kill|wait|update|rename|attach|exec|resize)`),
	r("DELETE", `/containers/`+id),

	r("POST", `/exec/`+id+`/(start|resize)`),
	r("GET", `/exec/`+id+`/json`),

	r("GET", `/images/json`),
	r("POST", `/images/create`),
	r("GET", `/images/`+ref+`/json`),
	r("DELETE", `/images/`+ref),
	r("GET", `/distribution/`+ref+`/json`),

	r("GET", `/networks`),
	r("GET DELETE", `/networks/`+id),
	r("POST", `/networks/create`),
	r("POST", `/networks/`+id+`/(connect|disconnect)`),

	r("GET", `/volumes`),
	r("GET DELETE", `/volumes/`+id),
	r("POST", `/volumes/create`),

	r("POST", `/build/prune`),
	r("POST", `/auth`),
}

var apiVersion = regexp.MustCompile(`^/v[0-9]+(\.[0-9]+)?/`)

// Allowed says whether the agent passes a Docker API call on. path is the
// request's path, with or without an API version (/v1.47/…).
func Allowed(method, path string) bool {
	if loc := apiVersion.FindStringIndex(path); loc != nil {
		path = path[loc[1]-1:]
	}
	// No dot segments or empty ones: what's matched is what Docker sees.
	for _, seg := range strings.Split(path[min(1, len(path)):], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, rl := range rules {
		if strings.Contains(rl.methods, " "+method+" ") && rl.path.MatchString(path) {
			return true
		}
	}
	return false
}
