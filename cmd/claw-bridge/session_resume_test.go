package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSessionLossReason(t *testing.T) {
	for _, reason := range []string{types.SessionLossReasonTurnTimeout, types.SessionLossReasonLockConflict, types.SessionLossReasonGatewayReconnect, types.SessionLossReasonProviderError, types.SessionLossReasonUnknown} {
		for _, err := range []error{&sessionPreservedError{err: errors.New("interrupted"), reason: reason}, &sessionRotatedError{err: errors.New("interrupted"), reason: reason}} {
			if got := sessionLossReason(fmt.Errorf("wrapped: %w", err)); got != reason {
				t.Errorf("reason = %q, want %q", got, reason)
			}
		}
	}
	for _, err := range []error{nil, errors.New("plain"), &sessionPreservedError{}, &sessionRotatedError{}} {
		if got := sessionLossReason(err); got != types.SessionLossReasonUnknown {
			t.Errorf("reason = %q, want unknown", got)
		}
	}
}

func TestSessionTranscriptLogBounds(t *testing.T) {
	var log sessionTranscriptLog
	for i := 0; i < 8; i++ {
		log.noteAssistant(fmt.Sprintf("assistant %d %s", i, strings.Repeat("a", 4096)))
	}
	for i := 0; i < 30; i++ {
		log.noteTool(agentActivity{Kind: "tool", Phase: "start", CallID: fmt.Sprint(i), Tool: "exec", Command: fmt.Sprintf("command %d %s", i, strings.Repeat("b", 4096))})
	}
	digest := log.snapshot()
	if len(digest.AssistantMessages) != 5 || len(digest.ToolCalls) != 20 || !digest.Truncated {
		t.Fatalf("digest sizes = %d/%d truncated=%v", len(digest.AssistantMessages), len(digest.ToolCalls), digest.Truncated)
	}
	if !strings.HasPrefix(digest.AssistantMessages[0], "assistant 3 ") || !strings.HasPrefix(digest.ToolCalls[0].Args, "command 10 ") {
		t.Fatal("oldest entries were not dropped")
	}
	data, err := json.Marshal(digest)
	if err != nil || len(data) > sessionDigestTotalBytes {
		t.Fatalf("digest = %d bytes, error %v", len(data), err)
	}
	// JSON escaping and multibyte runes must still respect the byte cap.
	for i := 0; i < 30; i++ {
		log.noteTool(agentActivity{Kind: "tool", Phase: "start", CallID: fmt.Sprintf("wide-%d", i), Tool: "exec", Command: strings.Repeat("界<&", 1000)})
	}
	data, err = json.Marshal(log.snapshot())
	if err != nil || len(data) > sessionDigestTotalBytes {
		t.Fatalf("wide digest = %d bytes, error %v", len(data), err)
	}
	if len(log.seen) > sessionDigestToolCallMax {
		t.Fatal("tool deduplication memory is not bounded")
	}
}

func TestSessionTranscriptLogRedactsAndTruncates(t *testing.T) {
	for _, input := range []string{`GH_TOKEN="secret"`, `GH_TOKEN='secret'`, `Bearer "abc"`, `GH_TOKEN="secret`} {
		t.Run(input, func(t *testing.T) {
			var log sessionTranscriptLog
			log.noteAssistant(input)
			log.noteTool(agentActivity{Kind: "tool", Phase: "running", Tool: "exec", Command: input})
			digest := log.snapshot()
			for _, got := range []string{digest.AssistantMessages[0], digest.ToolCalls[0].Args} {
				if strings.Contains(got, "secret") || strings.Contains(got, "abc") || !strings.HasSuffix(got, "[redacted]") {
					t.Fatalf("digest leaked quoted secret: %q", got)
				}
			}
		})
	}

	var log sessionTranscriptLog
	secret := "Bearer abc GITHUB_TOKEN=x "
	log.noteAssistant(secret + strings.Repeat("界", 700))
	log.noteTool(agentActivity{Kind: "tool", Phase: "running", Tool: "exec", Command: secret + strings.Repeat("界", 300)})
	digest := log.snapshot()
	text, args := digest.AssistantMessages[0], digest.ToolCalls[0].Args
	for _, value := range []string{text, args} {
		if strings.Contains(value, "Bearer abc") || strings.Contains(value, "GITHUB_TOKEN=x") || !strings.Contains(value, "Bearer [redacted] GITHUB_TOKEN=[redacted]") {
			t.Fatalf("unredacted content: %q", value)
		}
	}
	if utf8.RuneCountInString(text) != 600 || utf8.RuneCountInString(args) != 200 || !digest.Truncated {
		t.Fatalf("rune bounds = %d/%d truncated=%v", utf8.RuneCountInString(text), utf8.RuneCountInString(args), digest.Truncated)
	}
}

func TestSessionTranscriptLogDedupesToolCallByCallID(t *testing.T) {
	var log sessionTranscriptLog
	for _, phase := range []string{"start", "running", "completed"} {
		log.noteTool(agentActivity{Kind: "tool", Phase: phase, Tool: "exec", CallID: "a", Command: "echo start"})
	}
	log.noteTool(agentActivity{Kind: "tool", Phase: "completed", Tool: "read", CallID: "b", Path: "NOTES.md"})
	log.noteTool(agentActivity{Kind: "tool", Phase: "start", Tool: "noop", CallID: "c"})
	for _, phase := range []string{"start", "completed"} {
		log.noteTool(agentActivity{Kind: "tool", Phase: phase, Tool: "read", Path: "fallback.txt"})
	}
	log.noteTool(agentActivity{Kind: "model_wait", Phase: "start", Tool: "ignored"})
	log.noteTool(agentActivity{Kind: "tool", Phase: "pulse", Tool: "ignored"})
	digest := log.snapshot()
	if len(digest.ToolCalls) != 4 {
		t.Fatalf("tools = %#v", digest.ToolCalls)
	}
	// Snapshots do not expose the log's backing slices.
	digest.ToolCalls[0].Tool = "mutated"
	if log.snapshot().ToolCalls[0].Tool != "exec" {
		t.Fatal("snapshot mutated original")
	}
}

func TestSetSessionKeyStashesPreviousDigest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gs := &gatewaySession{}
	gs.setSessionKey("session-1")
	gs.transcript.noteAssistant("Progress")
	gs.transcript.noteTool(agentActivity{Kind: "tool", Phase: "start", Tool: "read", Path: "NOTES.md"})
	gs.setSessionKey("session-1")
	if gs.transcript.snapshot() == nil {
		t.Fatal("same key discarded history")
	}
	gs.setSessionKey("session-2")
	if gs.previousDigest == nil || gs.previousDigest.AssistantMessages[0] != "Progress" || len(gs.previousDigest.ToolCalls) != 1 {
		t.Fatalf("previous digest = %#v", gs.previousDigest)
	}
	if gs.transcript.snapshot() != nil || gs.transcript.key != "session-2" {
		t.Fatal("new session transcript not reset")
	}
}

func TestResolveSessionTranscriptPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldPath := sessionIndexPath
	sessionIndexPath = func() string { return filepath.Join(dir, "sessions.json") }
	t.Cleanup(func() { sessionIndexPath = oldPath })
	for _, entry := range []map[string]string{{"sessionFile": path}, {"sessionFile": "id.jsonl"}, {"sessionId": "id"}} {
		index, _ := json.Marshal(map[string]interface{}{"key": entry})
		if err := os.WriteFile(sessionIndexPath(), index, 0600); err != nil {
			t.Fatal(err)
		}
		if got := resolveSessionTranscriptPath("key"); got != path {
			t.Fatalf("path = %q, want %q", got, path)
		}
	}
	if got := resolveSessionTranscriptPath("missing"); got != "" {
		t.Fatalf("missing key = %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := resolveSessionTranscriptPath("key"); got != "" {
		t.Fatalf("missing transcript = %q", got)
	}
	if err := os.Remove(sessionIndexPath()); err != nil {
		t.Fatal(err)
	}
	if got := resolveSessionTranscriptPath("key"); got != "" {
		t.Fatalf("missing index = %q", got)
	}
}

func TestSessionTranscriptLogRedactsSecretAssignments(t *testing.T) {
	keys := []string{"token", "client_SECRET", "password", "db_passwd", "api_key", "apikey", "x-api-key", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "private_key", "credential", "Authorization"}
	forms := []string{`%s=SENSITIVE_VALUE`, `%s: SENSITIVE_VALUE`, `"%s":"SENSITIVE_VALUE"`, `'%s': 'SENSITIVE_VALUE'`, `%s="SENSITIVE_VALUE"`, `%s='SENSITIVE_VALUE'`, `"%s":SENSITIVE_VALUE`, `'%s': SENSITIVE_VALUE`, `%s: "SENSITIVE_VALUE"`, `%s: 'SENSITIVE_VALUE'`}
	for _, key := range keys {
		for _, form := range forms {
			input := fmt.Sprintf(form, key)
			t.Run(input, func(t *testing.T) {
				var log sessionTranscriptLog
				log.noteAssistant(input)
				log.noteTool(agentActivity{Kind: "tool", Phase: "start", Tool: "exec", Command: input})
				digest := log.snapshot()
				for _, got := range []string{sanitizeActivityText(input), digest.AssistantMessages[0], digest.ToolCalls[0].Args} {
					if strings.Contains(got, "SENSITIVE_VALUE") || !strings.Contains(got, "[redacted]") || !strings.Contains(got, key) {
						t.Fatalf("secret assignment not redacted: %q", got)
					}
				}
			})
		}
	}
}

func TestSanitizeActivityTextSecretAssignmentBoundaries(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{`{"api_key":"SENSITIVE_VALUE","path":"/workspace/main.go"}`, `{"api_key":[redacted],"path":"/workspace/main.go"}`},
		{"x-api-key: SENSITIVE_VALUE\nContent-Type: application/json", "x-api-key: [redacted]\nContent-Type: application/json"},
		{"Authorization: Basic SENSITIVE_VALUE", "Authorization: Basic [redacted]"},
		{"Authorization:\tBasic\tSENSITIVE_VALUE", "Authorization:\tBasic [redacted]"},
		{"authorization: bAsIc   SENSITIVE_VALUE", "authorization: Basic [redacted]"},
		{`curl -H "Authorization: Basic SENSITIVE_VALUE" /workspace/main.go`, `curl -H "Authorization: Basic [redacted]" /workspace/main.go`},
		{`api_key="first\"SENSITIVE_VALUE" path=main.go`, `api_key=[redacted] path=main.go`},
		{`api_key='first\'SENSITIVE_VALUE' path=main.go`, `api_key=[redacted] path=main.go`},
		{"token=SENSITIVE_VALUE,password=OTHER_VALUE", "token=[redacted],password=[redacted]"},
		{"token=SENSITIVE_VALUE;path=main.go", "token=[redacted];path=main.go"},
		{"path=/workspace/auth/token.go sha=0d9a1ad94add8a83839cc3bb3da0f1ff56f98152", "path=/workspace/auth/token.go sha=0d9a1ad94add8a83839cc3bb3da0f1ff56f98152"},
		{"/workspace/auth: normal path output", "/workspace/auth: normal path output"},
		{"https://example.com/api/token README.md", "https://example.com/api/token README.md"},
		{`{"path":"/workspace/main.go","sha":"abc123"}`, `{"path":"/workspace/main.go","sha":"abc123"}`},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if got := sanitizeActivityText(tt.input); got != tt.want {
				t.Fatalf("sanitized = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionTranscriptResolutionDoesNotHoldSessionMutex(t *testing.T) {
	for _, operation := range []string{"rotate", "preserved"} {
		t.Run(operation, func(t *testing.T) {
			gs := &gatewaySession{sessionKey: "old-session"}
			oldPath := sessionIndexPath
			calls := 0
			sessionIndexPath = func() string {
				calls++
				if !gs.sessionMu.TryLock() {
					t.Fatal("sessionMu held while resolving transcript path")
				}
				gs.sessionMu.Unlock()
				return filepath.Join(t.TempDir(), "missing.json")
			}
			t.Cleanup(func() { sessionIndexPath = oldPath })
			if operation == "rotate" {
				gs.setSessionKey("new-session")
			} else {
				edge := buildSessionRecoveryEdge(gs, &sessionPreservedError{}, "preserved")
				if edge.PreviousSessionKey != "old-session" {
					t.Fatalf("previous session = %q", edge.PreviousSessionKey)
				}
			}
			if calls == 0 {
				t.Fatal("transcript resolution was not attempted")
			}
		})
	}
}
