//go:build unix && !aix

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var errDaemonRunning = errors.New("another claw-bridge daemon is already running in this sandbox; refusing to start a second one")

func acquireDaemonLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		file, err = os.Open(path)
	}
	if err != nil {
		log.Printf("[bridge] warning: cannot open daemon lock %s; continuing without lock: %v", path, err)
		return nil, nil
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		defer file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			var buf [32]byte
			n, _ := file.ReadAt(buf[:], 0)
			if pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n]))); err == nil && pid > 0 {
				return nil, fmt.Errorf("%w (PID %d)", errDaemonRunning, pid)
			}
			return nil, errDaemonRunning
		}
		return nil, fmt.Errorf("lock claw-bridge daemon: %w", err)
	}
	// A read-only fallback can hold the lock without updating the PID.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	// Never unlink this file: every daemon must lock the same inode.
	return file, nil
}
