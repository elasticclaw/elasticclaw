package aichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	EventMessageStarted = "message_started"
	EventToken          = "token"
	EventBlock          = "block"
	EventToolStarted    = "tool_started"
	EventToolFinished   = "tool_finished"
	EventInterview      = "interview"
	EventAction         = "action"
	EventPanel          = "panel"
	EventDone           = "done"
	EventError          = "error"
)

type Emit func(string, any) error

type sseWriter struct {
	mu     sync.Mutex
	writer http.ResponseWriter
}

func (s *sseWriter) write(frame string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	controller := http.NewResponseController(s.writer)
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	if _, err := fmt.Fprint(s.writer, frame); err != nil {
		return err
	}
	return controller.Flush()
}
func (s *sseWriter) emit(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}

func streamWriter(w http.ResponseWriter) Emit { return (&sseWriter{writer: w}).emit }

func streamWithHeartbeat(ctx context.Context, w http.ResponseWriter, interval time.Duration, onError func()) (Emit, func()) {
	writer := &sseWriter{writer: w}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := writer.write(": ping\n\n"); err != nil {
					if onError != nil {
						onError()
					}
					return
				}
			}
		}
	}()
	return writer.emit, func() { cancel(); <-done }
}
