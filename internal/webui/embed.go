package webui

import (
	"embed"
	"io/fs"
)

//go:embed out
var files embed.FS

// FS returns the embedded Next.js static files rooted at the out/ directory.
func FS() (fs.FS, error) {
	return fs.Sub(files, "out")
}
