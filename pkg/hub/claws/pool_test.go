package claws

import (
	"sync"
	"testing"
	"time"
)

func TestWorkerPoolBoundsConcurrency(t *testing.T) {
	p := &WorkerPool{Limit: 2}
	release := make(chan struct{})
	var wg sync.WaitGroup

	// Fill the pool.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		if !p.TrySubmit(func() { defer wg.Done(); <-release }) {
			t.Fatalf("submit %d: pool rejected work below its limit", i)
		}
	}

	// Saturated pool must reject (backpressure signal for the WS loop).
	if p.TrySubmit(func() {}) {
		t.Fatal("expected TrySubmit to report overflow on a saturated pool")
	}

	// Once a worker finishes, capacity is available again.
	close(release)
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for {
		done := make(chan struct{})
		if p.TrySubmit(func() { close(done) }) {
			<-done
			return
		}
		select {
		case <-deadline:
			t.Fatal("pool never regained capacity after workers finished")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestWorkerPoolZeroValueUsesDefaultLimit(t *testing.T) {
	p := &WorkerPool{}
	done := make(chan struct{})
	if !p.TrySubmit(func() { close(done) }) {
		t.Fatal("zero-value pool rejected work")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("submitted work never ran")
	}
}
