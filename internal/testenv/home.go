// Package testenv keeps test binaries away from the developer's machine.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const isolatedHomeEnv = "ELASTICCLAW_TEST_ISOLATED_HOME"

// RunWithIsolatedHome runs the package's tests with HOME and the hub config
// pointing at a throwaway directory, so tests never load real hub
// credentials (for example GitHub Apps under ~/.elasticclaw/workspaces) or
// write into the developer's home. The directory is removed afterwards.
//
// Call it from TestMain: os.Exit(testenv.RunWithIsolatedHome(m)).
func RunWithIsolatedHome(m *testing.M) int {
	// Tests that re-exec the test binary as a helper process inherit the
	// parent's isolated HOME; reuse it, since the child may exit without
	// running deferred cleanup.
	if parent := os.Getenv(isolatedHomeEnv); parent != "" && parent == os.Getenv("HOME") {
		return m.Run()
	}

	home, err := os.MkdirTemp("", "elasticclaw-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testenv: create isolated HOME: %v\n", err)
		return 1
	}
	defer os.RemoveAll(home)

	os.Setenv(isolatedHomeEnv, home)
	os.Setenv("HOME", home)
	os.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(home, ".elasticclaw", "hub.yaml"))
	return m.Run()
}
