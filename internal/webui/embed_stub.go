//go:build !embedweb

package webui

import (
	"errors"
	"io/fs"
)

// FS returns an error when the web UI is not embedded (build without -tags embedweb).
func FS() (fs.FS, error) {
	return nil, errors.New("web UI not embedded — build with: make build-release")
}
