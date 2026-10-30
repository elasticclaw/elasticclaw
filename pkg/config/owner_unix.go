//go:build unix

package config

import (
	"os"
	"syscall"
)

// preserveOwner gives f the owner of info when running as root, so a root-run
// command does not lock a non-root hub out of its own config.
func preserveOwner(f *os.File, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return f.Chown(int(st.Uid), int(st.Gid))
}
