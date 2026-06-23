package hub

import (
	"strings"
	"testing"
)

func TestCaptureModelAuthOutputStripsANSIFromURL(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("\x1b[32mhttps://auth.openai.com/codex/device_authorization\x1b[0m\n"), make(chan struct{}, 1))

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want stripped device authorization URL", job.URL)
	}
	if strings.Contains(job.Output, "\x1b") || strings.Contains(job.Output, "[0m") {
		t.Fatalf("Output = %q, want ANSI escape sequences stripped", job.Output)
	}
}

func TestCaptureModelAuthOutputDoesNotTreatCodexURLAsCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device_authorization\n"), make(chan struct{}, 1))

	if job.Code != "" {
		t.Fatalf("Code = %q, want no code extracted from codex URL", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsDeviceCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("User code: ABCD-EFGH\n"), make(chan struct{}, 1))

	if job.Code != "ABCD-EFGH" {
		t.Fatalf("Code = %q, want device code", job.Code)
	}
}
