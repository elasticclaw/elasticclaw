package aichat

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHeartbeatSerializesFramesAndStops(t *testing.T) {
	rec := httptest.NewRecorder()
	emit, stop := streamWithHeartbeat(context.Background(), rec, time.Millisecond, nil)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := emit(EventToken, map[string]string{"text": "hello"}); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	wg.Wait()
	stop()
	output := rec.Body.String()
	if !strings.Contains(output, ": ping\n\n") {
		t.Fatal("missing heartbeat")
	}
	frames := strings.Split(strings.TrimSuffix(output, "\n\n"), "\n\n")
	tokens := 0
	for _, frame := range frames {
		if frame == ": ping" {
			continue
		}
		if frame != "event: token\ndata: {\"text\":\"hello\"}" {
			t.Fatalf("interleaved frame: %q", frame)
		}
		tokens++
	}
	if tokens != 20 {
		t.Fatalf("tokens = %d", tokens)
	}
	time.Sleep(3 * time.Millisecond)
	if rec.Body.String() != output {
		t.Fatal("heartbeat continued after stop")
	}
	stop()
}
