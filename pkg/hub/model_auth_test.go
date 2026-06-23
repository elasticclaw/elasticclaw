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

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device_authorization\nCode: authorization\n"), make(chan struct{}, 1))

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

func TestCaptureModelAuthOutputExtractsNumericDeviceCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Authorization code: 123-456-789\n"), make(chan struct{}, 1))

	if job.Code != "123-456-789" {
		t.Fatalf("Code = %q, want numeric device code", job.Code)
	}
}

func TestAppendModelAuthOutputReplacesBadCodeWithRealCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.appendModelAuthOutput(job, "Code: authorization\n")
	s.appendModelAuthOutput(job, "Authorization code: 123-456-789\n")

	if job.Code != "123-456-789" {
		t.Fatalf("Code = %q, want real code after rejected prose token", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsURLWithoutNewline(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device_authorization"), make(chan struct{}, 1))

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want URL before process exits without newline", job.URL)
	}
}

func TestAppendModelAuthOutputExtractsSplitURL(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.appendModelAuthOutput(job, "Open https://auth.openai.com/codex/")
	s.appendModelAuthOutput(job, "device_authorization")

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want URL reconstructed from streamed chunks", job.URL)
	}
}

func TestAppendModelAuthOutputExtractsOSCURL(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.appendModelAuthOutput(job, "\x1b]8;;https://auth.openai.com/codex/device_authorization\x07click here\x1b]8;;\x07")

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want URL from terminal hyperlink", job.URL)
	}
	if strings.Contains(job.Output, "\x1b") {
		t.Fatalf("Output = %q, want terminal hyperlink escapes stripped", job.Output)
	}
}
