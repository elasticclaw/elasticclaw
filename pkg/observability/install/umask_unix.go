//go:build linux || darwin

package install

import "golang.org/x/sys/unix"

// setUmask returns a function that restores the previous mask.
func setUmask(mask int) func() {
	old := unix.Umask(mask)
	return func() { unix.Umask(old) }
}
