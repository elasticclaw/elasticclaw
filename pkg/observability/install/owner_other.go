//go:build !unix

package install

import "os"

func chownLike(string, os.FileInfo) error { return nil }
