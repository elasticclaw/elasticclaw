//go:build unix && !aix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonLockExcludesSecondDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.lock")
	first, err := acquireDaemonLock(path)
	if err != nil || first == nil {
		t.Fatalf("first acquisition: file=%v err=%v", first, err)
	}
	t.Cleanup(func() { first.Close() })
	if second, err := acquireDaemonLock(path); !errors.Is(err, errDaemonRunning) || second != nil {
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
