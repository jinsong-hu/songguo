// Package web embeds the built React dashboard so production builds ship as a
// single binary. The repository keeps a placeholder in dist so Go commands still
// compile on a clean checkout; release builds generate the real bundle first.
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
