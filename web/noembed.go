//go:build !embedweb

package web

import "io/fs"

// Dist returns nil: this build doesn't embed the frontend. Build with
// -tags embedweb to include it, or serve web/dist with HLM_WEB_DIR.
func Dist() fs.FS { return nil }
