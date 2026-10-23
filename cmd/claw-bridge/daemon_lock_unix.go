//go:build unix && !aix

package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/unix"
)

var errDaemonRunning = errors.New("another claw-bridge daemon is already running in this sandbox; refusing to start a second one")

func acquireDaemonLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		file, err = os.Open(path)
	}
	if err != nil {
		log.Printf("[bridge] warning: cannot open daemon lock %s; continuing without lock: %v", path, err)
		return nil, nil
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errDaemonRunning
		}
		return nil, fmt.Errorf("lock claw-bridge daemon: %w", err)
	}
	// Never unlink this file: every daemon must lock the same inode.
	return file, nil
}
