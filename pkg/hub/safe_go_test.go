package hub

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestSafeGoRecoversAndLogsPanic(t *testing.T) {
	var logs lockedBuffer
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
