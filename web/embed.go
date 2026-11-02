//go:build embedweb

// Package web holds the built frontend. Release builds embed web/dist into
// the binary with the embedweb build tag (run `npm run build` first), so a
// single file is all a binary install needs.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the embedded frontend, or nil if it wasn't embedded.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	return sub
}
