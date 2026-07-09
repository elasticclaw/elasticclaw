package claws

import (
	"sync"

	"golang.org/x/sync/semaphore"
	"nhooyr.io/websocket"
)

// DefaultWSWorkerLimit is the default bound on concurrent WS message
// handlers per hub (phase-2 item 2.3: the former go-per-message spawn had
// no bound).
const DefaultWSWorkerLimit = 64

// StatusOverloaded is the WebSocket close code the hub sends when a
// connection submits work while the worker pool is saturated. It is in the
// private-use range (4000-4999) so bridges can distinguish overload from
// policy violations and back off before reconnecting.
const StatusOverloaded = websocket.StatusCode(4429)

// WorkerPool bounds the goroutines spawned to handle claw WebSocket
// messages using a weighted semaphore. The zero value is ready to use
// (hand-built test servers construct the hub Server without wiring) and
// applies DefaultWSWorkerLimit; set Limit before first use to override.
type WorkerPool struct {
	// Limit is the maximum number of concurrently running handlers.
	// <= 0 means DefaultWSWorkerLimit. Read once, on first submission.
	Limit int64

	once sync.Once
	sem  *semaphore.Weighted
}

func (p *WorkerPool) init() {
	p.once.Do(func() {
		limit := p.Limit
		if limit <= 0 {
			limit = DefaultWSWorkerLimit
		}
		p.sem = semaphore.NewWeighted(limit)
	})
}

// TrySubmit runs fn on its own goroutine if the pool has capacity and
// reports whether it did. A false return means the pool is saturated —
// the queue overflowed — and the caller should apply backpressure (the WS
// read loop closes the connection with StatusOverloaded).
func (p *WorkerPool) TrySubmit(fn func()) bool {
	p.init()
	if !p.sem.TryAcquire(1) {
		return false
	}
	go func() {
		defer p.sem.Release(1)
		fn()
	}()
	return true
}
