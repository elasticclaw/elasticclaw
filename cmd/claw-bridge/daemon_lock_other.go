//go:build !unix || aix

package main

import "os"

func acquireDaemonLock(path string) (*os.File, error) {
	return nil, nil
}
