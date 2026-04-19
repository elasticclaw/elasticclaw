package webui

import (
	"embed"
	"errors"
	"io/fs"
)

// files embeds the Next.js static export from web/out/ (copied here by make build-web).
// The out/ directory must exist with at least a README.md for this to compile.
// Run `make build-web` or `make build-release` to populate it with the real UI.
//
//go:embed out
var files embed.FS

// FS returns the embedded Next.js static files rooted at the out/ directory.
// Returns an error if the web UI was not built yet (only README.md present).
func FS() (fs.FS, error) {
	sub, err := fs.Sub(files, "out")
	if err != nil {
		return nil, err
	}
	// If only README.md exists, web was not built
	if _, err := sub.Open("index.html"); err != nil {
		return nil, errors.New("web UI not built — run: make build-web")
	}
	return sub, nil
}
