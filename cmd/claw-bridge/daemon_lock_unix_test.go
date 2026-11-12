//go:build unix && !aix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDaemonLockExcludesSecondDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.lock")
	first, err := acquireDaemonLock(path)
	if err != nil || first == nil {
		t.Fatalf("first acquisition: file=%v err=%v", first, err)
	}
	t.Cleanup(func() { first.Close() })
	info, err := first.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("lock permissions: %v, %v", info, err)
	}
	if second, err := acquireDaemonLock(path); !errors.Is(err, errDaemonRunning) || second != nil || !strings.Contains(err.Error(), "PID "+strconv.Itoa(os.Getpid())) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second acquisition: file=%v err=%v", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireDaemonLock(path)
	if err != nil || third == nil {
		t.Fatalf("acquisition after close: file=%v err=%v", third, err)
	}
	third.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock inode must remain after close: %v", err)
	}
}

func TestDaemonLockPathIsFixed(t *testing.T) {
	// TMPDIR and HOME differ across exec shells in a sandbox, so the daemon
	// lock must not be derived from either.
	if daemonLockPath != "/tmp/elasticclaw-claw-bridge.lock" {
		t.Fatalf("daemonLockPath = %q", daemonLockPath)
	}
}

func TestDaemonLockFailsOpenWhenFileCannotBeOpened(t *testing.T) {
	file, err := acquireDaemonLock(filepath.Join(t.TempDir(), "missing", "bridge.lock"))
	if err != nil || file != nil {
		t.Fatalf("unopenable lock: file=%v err=%v", file, err)
	}
}

func TestDaemonLockReadOnlyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.lock")
	if err := os.WriteFile(path, nil, 0444); err != nil {
		t.Fatal(err)
	}
	first, err := acquireDaemonLock(path)
	if err != nil || first == nil {
		t.Fatalf("read-only acquisition: file=%v err=%v", first, err)
	}
	defer first.Close()
	if second, err := acquireDaemonLock(path); !errors.Is(err, errDaemonRunning) || second != nil {
		if second != nil {
			second.Close()
		}
		t.Fatalf("read-only second acquisition: file=%v err=%v", second, err)
	}
}

func TestDaemonLockTruncatesOldPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.lock")
	if err := os.WriteFile(path, []byte("12345678901234567890\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := acquireDaemonLock(path)
	if err != nil || file == nil {
		t.Fatalf("acquire: file=%v err=%v", file, err)
	}
	defer file.Close()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock PID = %q, err=%v", data, err)
	}
}
