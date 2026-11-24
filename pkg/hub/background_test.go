package hub

import (
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopBackgroundWaitsForRunningGoroutines(t *testing.T) {
	s := &Server{}
	var finished atomic.Bool
	s.goBG("slow worker", func() {
		<-s.bgContext().Done()
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
	})

	if !s.stopBackground(testWaitTimeout) {
		t.Fatal("stopBackground timed out on a goroutine that honours bgContext")
	}
	if !finished.Load() {
		t.Fatal("stopBackground returned before the goroutine finished")
	}
}

func TestGoBGIsNoOpAfterStop(t *testing.T) {
	s := &Server{}
	s.stopBackground(testWaitTimeout)

	ran := make(chan struct{})
	s.goBG("late worker", func() { close(ran) })
	select {
	case <-ran:
		t.Fatal("goBG started a goroutine after stopBackground")
	case <-time.After(50 * time.Millisecond):
	}
	if s.bgSleep(time.Minute) {
		t.Fatal("bgSleep slept through a stopped server")
	}
}

func TestStopBackgroundNamesStuckGoroutines(t *testing.T) {
	var logs lockedBuffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	s := &Server{}
	release := make(chan struct{})
	s.goBG("stuck worker", func() { <-release })

	if s.stopBackground(20 * time.Millisecond) {
		t.Fatal("stopBackground reported success with a goroutine still running")
	}
	if got := logs.String(); !strings.Contains(got, "still running after 20ms: stuck worker") {
		t.Fatalf("stuck goroutine was not named in the log: %q", got)
	}
	close(release)
	if !s.stopBackground(testWaitTimeout) {
		t.Fatal("stopBackground still timed out after the goroutine was released")
	}
}
