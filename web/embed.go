// Package web embeds the built React dashboard so the gateway ships as a single
// binary. The dist directory is produced by `npm run build` in this folder and
// is committed to the repository so the Go build does not require Node.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded built dashboard rooted at the dist directory.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
