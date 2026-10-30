//go:build unix

package install

import (
	"os"
	"syscall"
)

// chownLike gives path the owner and group recorded in info.
func chownLike(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(stat.Uid), int(stat.Gid)); err != nil && !os.IsPermission(err) {
		return err
	}
	return nil
}
