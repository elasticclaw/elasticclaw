package hub

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

func TestWithRecoveryPanic(t *testing.T) {
	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	h := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var envelope apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not valid JSON: %v (body: %q)", err, rec.Body.String())
	}
	if envelope.Error != "internal server error" || envelope.Code != "internal" {
		t.Fatalf("envelope = %+v, want error=internal server error code=internal", envelope)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "panic recovered: boom") {
		t.Fatalf("log missing panic message: %q", logged)
	}
	if !strings.Contains(logged, "GET /api/test") {
		t.Fatalf("log missing request info: %q", logged)
	}
	if !strings.Contains(logged, "middleware_test.go") {
		t.Fatalf("log missing stack trace: %q", logged)
	}
}

func TestWithRecoveryPassthrough(t *testing.T) {
	h := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot || rec.Body.String() != "ok" {
		t.Fatalf("response altered by middleware: %d %q", rec.Code, rec.Body.String())
	}
}

func TestWithRecoveryRepanicsOnAbortHandler(t *testing.T) {
	h := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("recover() = %v, want http.ErrAbortHandler", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestWriteErr(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusNotFound, "not_found", "claw not found")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var envelope apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if envelope.Error != "claw not found" || envelope.Code != "not_found" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&buf, nil))}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.FromContext(r.Context()).Info("first")
		logger.FromContext(r.Context()).Info("second")
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	s.withRequestID(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/claws", nil))

	id := rec.Header().Get("X-Request-ID")
	if len(id) != 8 {
		t.Fatalf("expected 8-char X-Request-ID header, got %q", id)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %q", len(lines), buf.String())
	}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if entry["request_id"] != id {
			t.Fatalf("log line request_id=%v does not match header %q", entry["request_id"], id)
		}
	}
}

func TestLogfComponentExtraction(t *testing.T) {
	var buf bytes.Buffer
	logMsg(slog.New(slog.NewJSONHandler(&buf, nil)), "[claw] instance %s started", "abc")
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", buf.String())
	}
	if entry["component"] != "claw" {
		t.Fatalf("expected component=claw, got %v", entry["component"])
	}
	if entry["msg"] != "instance abc started" {
		t.Fatalf("unexpected message: %v", entry["msg"])
	}

	buf.Reset()
	entry = map[string]any{}
	logMsg(slog.New(slog.NewJSONHandler(&buf, nil)), "no tag here %d", 7)
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", buf.String())
	}
	if _, ok := entry["component"]; ok {
		t.Fatalf("did not expect a component attribute: %v", entry)
	}
	if entry["msg"] != "no tag here 7" {
		t.Fatalf("unexpected message: %v", entry["msg"])
	}
}

func TestWriteErrOmitsEmptyCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusBadRequest, "", "bad input")

	if strings.Contains(rec.Body.String(), "code") {
		t.Fatalf("empty code should be omitted from envelope: %q", rec.Body.String())
	}
}
