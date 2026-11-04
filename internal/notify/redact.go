package notify

import "regexp"

var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// user:password@ in URLs.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}***@"},
	// Secret-looking query parameters and key=value pairs.
	{regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:token|secret|password|passwd|pwd|apikey|api_key|access_key|signature|auth)[a-z0-9_.-]*=)[^\s&"']+`), "${1}***"},
	// Bearer tokens.
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}***"},
	// Discord webhook tokens.
	{regexp.MustCompile(`(?i)(/api/webhooks/\d+/)[A-Za-z0-9_-]+`), "${1}***"},
}

// Redact hides credentials that error messages sometimes carry, such as
// passwords in URLs and tokens in query strings, before they're sent
// anywhere.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}
