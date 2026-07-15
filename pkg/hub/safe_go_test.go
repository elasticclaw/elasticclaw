package hub

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestSafeGoRecoversAndLogsPanic(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	done := make(chan struct{})
	(&Server{}).safeGo("test worker", func() {
		defer close(done)
		panic("malformed payload")
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("safeGo function did not run")
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "panic in test worker: malformed payload") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := logs.String(); !strings.Contains(got, "panic in test worker: malformed payload") || !strings.Contains(got, "goroutine ") {
		t.Fatalf("panic was not logged with stack: %q", got)
	}
}
