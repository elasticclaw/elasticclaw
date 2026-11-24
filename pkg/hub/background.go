package hub

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// backgroundTasks tracks the goroutines that belong to a Server's lifetime, so
// shutdown (and every test) can stop them before the database closes instead of
// letting them run against a closed DB or a removed temp directory.
// The zero value is ready to use.
type backgroundTasks struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	nextID  uint64
	running map[uint64]string
}

func (b *backgroundTasks) initLocked() {
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithCancel(context.Background())
		b.running = make(map[uint64]string)
	}
}

// start registers a task and returns the function that marks it finished.
// It reports false once stop has been called: nothing new may start then.
func (b *backgroundTasks) start(name string) (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initLocked()
	if b.stopped {
		return nil, false
	}
	b.nextID++
	id := b.nextID
	b.running[id] = name
	b.wg.Add(1)
	return func() {
		b.mu.Lock()
		delete(b.running, id)
		b.mu.Unlock()
		b.wg.Done()
	}, true
}

func (b *backgroundTasks) runningNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.running))
	for _, name := range b.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// bgContext is cancelled when the Server stops its background goroutines.
// Background work that sleeps or waits should select on it.
func (s *Server) bgContext() context.Context {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	s.bg.initLocked()
	return s.bg.ctx
}

// goBG runs fn in a goroutine owned by the Server: stopBackground waits for
// it. After stopBackground the call is a no-op, because whatever fn would
// touch (the database first of all) is going away.
func (s *Server) goBG(name string, fn func()) {
	done, ok := s.bg.start(name)
	if !ok {
		return
	}
	go func() {
		defer done()
		fn()
	}()
}

// bgSleep waits for d and reports true, or reports false as soon as the Server
// stops its background goroutines.
func (s *Server) bgSleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.bgContext().Done():
		return false
	}
}

// stopBackground cancels bgContext, refuses new goBG work, and waits up to
// timeout for the running goroutines. It reports whether all of them finished;
// the ones still running are logged so a stuck goroutine is easy to name.
func (s *Server) stopBackground(timeout time.Duration) bool {
	s.bg.mu.Lock()
	s.bg.initLocked()
	s.bg.stopped = true
	s.bg.cancel()
	s.bg.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		s.bg.wg.Wait()
		close(finished)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
		return true
	case <-timer.C:
		log.Printf("[hub] background goroutines still running after %s: %s", timeout, strings.Join(s.bg.runningNames(), ", "))
		return false
	}
}
