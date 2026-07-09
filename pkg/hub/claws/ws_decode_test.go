package claws

import (
	"encoding/json"
	"testing"
)

// TestClawWSDecoders verifies the typed-payload decoder registry (phase-2
// item 2.5): every claw-bound message type has a decoder, malformed payloads
// come back as protocol errors (never a panic or a silent nil handler), and
// well-formed payloads yield a ready-to-run handler without touching the
// Service. Handlers are not invoked — decode is the unit under test.
func TestClawWSDecoders(t *testing.T) {
	sess := &clawSession{} // decode must not need Service state
	decoders := sess.decoders()

	wantTypes := []string{
		"heartbeat", "agent_activity", "chunk", "message",
		"file_ack", "file_read_resp", "volume_attach_ack", "volume_sync_ack",
		"http_proxy_req",
	}
	for _, typ := range wantTypes {
		if _, ok := decoders[typ]; !ok {
			t.Errorf("no decoder registered for %q", typ)
		}
	}
	if len(decoders) != len(wantTypes) {
		t.Errorf("registry has %d decoders, want %d", len(decoders), len(wantTypes))
	}

	valid := map[string]string{
		"heartbeat":         `{"gateway_healthy":true,"context_usage":42}`,
		"agent_activity":    `{"kind":"tool","tool":"exec"}`,
		"chunk":             `{"content":"hello"}`,
		"message":           `{"content":"done"}`,
		"file_ack":          `{"request_id":"r1","ok":true}`,
		"file_read_resp":    `{"request_id":"r1","ok":true}`,
		"volume_attach_ack": `{"request_id":"r1","lease_id":"l1","ok":true}`,
		"volume_sync_ack":   `{"request_id":"r1","lease_id":"l1","ok":true}`,
		"http_proxy_req":    `{"req_id":"q1","method":"GET","path":"/api/claws"}`,
	}
	for typ, payload := range valid {
		handle, err := decoders[typ](json.RawMessage(payload))
		if err != nil {
			t.Errorf("decode %q with valid payload: unexpected error %v", typ, err)
		}
		if handle == nil {
			t.Errorf("decode %q with valid payload: nil handler", typ)
		}
	}

	for typ := range decoders {
		handle, err := decoders[typ](json.RawMessage(`"not an object"`))
		if err == nil {
			t.Errorf("decode %q with malformed payload: want protocol error, got handler=%v", typ, handle)
		}
	}
}
