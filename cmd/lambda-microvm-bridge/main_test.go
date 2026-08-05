package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHookStagesFilesAndStartsBridgeWithRuntimeEnv(t *testing.T) {
	temp := t.TempDir()
	bridge := filepath.Join(temp, "fake-claw-bridge")
	started := filepath.Join(temp, "started")
	script := "#!/bin/sh\nprintf '%s|%s|%s' \"$1\" \"$ELASTICCLAW_CLAW_ID\" \"$ELASTICCLAW_MICROVM_ID\" > \"$STARTED_FILE\"\n"
	if err := os.WriteFile(bridge, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	s := &server{bridgeBinary: bridge, workspaceDir: filepath.Join(temp, "workspace")}
	payload, err := json.Marshal(runPayload{
		Version: 1,
		Env: map[string]string{
			"ELASTICCLAW_CLAW_ID": "claw-123",
			"STARTED_FILE":        started,
		},
		TemplateFiles: map[string]string{
			"AGENTS.md": base64.StdEncoding.EncodeToString([]byte("instructions")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runPayload, err := json.Marshal(runPayload{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(lifecycleRequest{MicroVMID: "mvm-123", RunHookPayload: string(runPayload)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/aws/lambda-microvms/runtime/v1/run", bytes.NewReader(body))
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/elasticclaw/v1/init", bytes.NewReader(payload))
	response = httptest.NewRecorder()
	s.routes().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("init status = %d, body = %s", response.Code, response.Body.String())
	}
	t.Cleanup(s.shutdownBridge)

	file, err := os.ReadFile(filepath.Join(s.workspaceDir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(file) != "instructions" {
		t.Fatalf("workspace file = %q", file)
	}
	if _, err := os.Stat(filepath.Join(s.workspaceDir, ".elasticclaw-workspace-ready")); err != nil {
		t.Fatalf("workspace readiness marker: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		contents, readErr := os.ReadFile(started)
		if got, want := string(contents), "--bootstrap|claw-123|mvm-123"; readErr == nil && got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge invocation = %q, error = %v, want %q", contents, readErr, "--bootstrap|claw-123|mvm-123")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunHookRejectsEscapingTemplatePath(t *testing.T) {
	s := &server{bridgeBinary: "/does/not/matter", workspaceDir: t.TempDir()}
	payload, err := json.Marshal(runPayload{
		Version: 1,
		TemplateFiles: map[string]string{
			"../secret": base64.StdEncoding.EncodeToString([]byte("nope")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/elasticclaw/v1/init", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.workspaceDir, "..", "secret")); !os.IsNotExist(err) {
		t.Fatalf("escaping file was created")
	}
}

func TestExecUsesRuntimeEnvironment(t *testing.T) {
	s := &server{runtimeEnv: mergeEnv(os.Environ(), map[string]string{"ELASTICCLAW_TEST_VALUE": "available"})}
	body := `{"command":["sh","-c","printf %s \"$ELASTICCLAW_TEST_VALUE\""]}`
	request := httptest.NewRequest(http.MethodPost, "/elasticclaw/v1/exec", strings.NewReader(body))
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result execResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "available" {
		t.Fatalf("exec result = %+v", result)
	}
}

func TestCleanRelativePath(t *testing.T) {
	for _, path := range []string{"", "/etc/passwd", "../secret", "nested/../../secret"} {
		if _, err := cleanRelativePath(path); err == nil {
			t.Errorf("cleanRelativePath(%q) unexpectedly succeeded", path)
		}
	}
	if got, err := cleanRelativePath("scripts/check.sh"); err != nil || got != filepath.Join("scripts", "check.sh") {
		t.Fatalf("cleanRelativePath valid = %q, %v", got, err)
	}
}
