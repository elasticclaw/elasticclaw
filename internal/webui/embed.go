//go:build embedweb

package webui

import (
	"embed"
	"errors"
	"io/fs"
)

//go:embed all:out
var files embed.FS

// FS returns the embedded Next.js static files.
func FS() (fs.FS, error) {
	sub, err := fs.Sub(files, "out")
	if err != nil {
		return nil, err
	}
	if _, err := sub.Open("index.html"); err != nil {
		return nil, errors.New("web UI not built")
	}
	return sub, nil
}
