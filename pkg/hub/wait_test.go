package hub

import (
	"testing"
	"time"
)

// testWaitTimeout is how long a test waits for asynchronous hub work before it
// fails. A wait returns as soon as its condition holds, so a generous deadline
// costs nothing on a passing run; the 2s this used to be was too short on a
// loaded 2-CPU CI runner.
const testWaitTimeout = 15 * time.Second

// eventually polls check until it reports true and fails the test when it has
// not done so within testWaitTimeout.
func eventually(t testing.TB, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWaitTimeout)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
