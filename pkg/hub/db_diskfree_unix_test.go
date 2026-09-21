//go:build linux || darwin

package hub

import (
	"runtime"
	"testing"
)

// db_diskfree_unix.go is selected by the `unix` build constraint; this file by
// an explicit GOOS list. The two disagree the moment that constraint stops
// matching a platform the hub runs on -- a typo in the tag, a fallback file
// that starts claiming Linux -- and the boot-time allocation gate then
// silently answers "unknown" everywhere. TestDiskFreeBytesQueriesTheFilesystem
// skips when the fallback answers; this one fails.
func TestDiskFreeBytesOSIsTheStatfsImplementationOnUnix(t *testing.T) {
	free, err := diskFreeBytesOS(t.TempDir())
	if err != nil {
		t.Fatalf("diskFreeBytesOS on %s answered with the fallback: %v (the unix build constraint no longer selects db_diskfree_unix.go)", runtime.GOOS, err)
	}
	if free <= 0 {
		t.Fatalf("free = %d on a filesystem a test directory was just created on", free)
	}
}
