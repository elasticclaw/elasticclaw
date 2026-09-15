//go:build unix

package hub

import "syscall"

// diskFreeBytesOS is statfs on the directory: the blocks available to an
// unprivileged writer, not the ones the kernel reserves for root.
func diskFreeBytesOS(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
