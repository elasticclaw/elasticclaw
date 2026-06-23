package lambdamicrovms

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestRunMicroVMArgs(t *testing.T) {
	autoResume := true
	p, err := New(Config{
		ImageIdentifier:          "arn:aws:lambda:us-east-1:123456789012:microvm-image:elasticclaw",
		ImageVersion:             "1.0",
		ExecutionRoleARN:         "arn:aws:iam::123456789012:role/MicroVMExecutionRole",
		IngressNetworkConnectors: []string{"arn:aws:lambda:us-east-1:aws:network-connector:aws-network-connector:ALL_INGRESS"},
		EgressNetworkConnectors:  []string{"arn:aws:lambda:us-east-1:aws:network-connector:aws-network-connector:INTERNET_EGRESS"},
		IdleMaxDurationSeconds:   900,
		SuspendedDurationSeconds: 300,
		AutoResume:               &autoResume,
		MaximumDurationSeconds:   14400,
	})
	if err != nil {
		t.Fatal(err)
	}

	args, err := p.runMicroVMArgs("file:///tmp/elasticclaw-run-hook.json")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")

	for _, want := range []string{
		"lambda-microvms", "run-microvm",
		"--image-identifier", "arn:aws:lambda:us-east-1:123456789012:microvm-image:elasticclaw",
		"--image-version", "1.0",
		"--execution-role-arn", "arn:aws:iam::123456789012:role/MicroVMExecutionRole",
		"--ingress-network-connectors", "arn:aws:lambda:us-east-1:aws:network-connector:aws-network-connector:ALL_INGRESS",
		"--egress-network-connectors", "arn:aws:lambda:us-east-1:aws:network-connector:aws-network-connector:INTERNET_EGRESS",
		"--maximum-duration-in-seconds", "14400",
		"--run-hook-payload", "file:///tmp/elasticclaw-run-hook.json",
		"--output", "json",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if !strings.Contains(joined, `"autoResumeEnabled":true`) || !strings.Contains(joined, `"maxIdleDurationSeconds":900`) {
		t.Fatalf("args %q missing idle policy JSON", args)
	}
}

func TestWriteRunHookPayloadFileKeepsPayloadOutOfArgs(t *testing.T) {
	payload := `{"env":{"ELASTICCLAW_CLAW_TOKEN":"secret-token"}}`

	arg, cleanup, err := writeRunHookPayloadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if !strings.HasPrefix(arg, "file://") {
		t.Fatalf("payload arg = %q, want file:// reference", arg)
	}
	if strings.Contains(arg, "secret-token") {
		t.Fatalf("payload arg leaks secret: %q", arg)
	}
	path := strings.TrimPrefix(arg, "file://")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Fatalf("payload file = %q, want original payload", string(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("payload file permissions = %v, want 0600", got)
	}
}

func TestBuildRunHookPayloadEncodesTemplateFiles(t *testing.T) {
	payload, err := buildRunHookPayload(types.CreateRequest{
		Name: "ec-test",
		Env:  map[string]string{"ELASTICCLAW_CLAW_ID": "claw-1"},
		TemplateFiles: map[string][]byte{
			"README.md": []byte("hello"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded runHookPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TemplateFiles["README.md"] != "aGVsbG8=" {
		t.Fatalf("encoded README = %q, want base64 content", decoded.TemplateFiles["README.md"])
	}
}

func TestBuildRunHookPayloadRejectsOversizedPayload(t *testing.T) {
	_, err := buildRunHookPayload(types.CreateRequest{
		Name: "ec-test",
		TemplateFiles: map[string][]byte{
			"large.txt": []byte(strings.Repeat("x", 17*1024)),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds 16KiB") {
		t.Fatalf("err = %v, want payload size error", err)
	}
}
