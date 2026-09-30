package aichat

import (
	"encoding/json"
	"fmt"
	"net/http"
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

func streamWriter(w http.ResponseWriter) Emit {
	return func(event string, payload any) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		defer controller.SetWriteDeadline(time.Time{})
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		return controller.Flush()
	}
}
