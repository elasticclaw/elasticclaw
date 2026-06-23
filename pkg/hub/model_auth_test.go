package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelAuthCLIUsesPinnedVersions(t *testing.T) {
	t.Setenv("ELASTICCLAW_CODEX_CLI_VERSION", "1.2.3")
	t.Setenv("ELASTICCLAW_GROK_CLI_VERSION", "4.5.6")

	codex, err := modelAuthCLI("codex")
	if err != nil {
		t.Fatal(err)
	}
	if codex.PackageName != "@openai/codex" || codex.Version != "1.2.3" || codex.BinaryName != "codex" {
		t.Fatalf("codex CLI = %#v", codex)
	}
	if got := strings.Join(codex.LoginArgs, " "); got != "login --device-auth" {
		t.Fatalf("codex login args = %q", got)
	}

	grok, err := modelAuthCLI("grok")
	if err != nil {
		t.Fatal(err)
	}
	if grok.PackageName != "@xai-official/grok" || grok.Version != "4.5.6" || grok.BinaryName != "grok" {
		t.Fatalf("grok CLI = %#v", grok)
	}
}

func TestEnsureModelAuthCLIUsesManagedBinary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ELASTICCLAW_MODEL_CLI_DIR", filepath.Join(root, "model-clis"))
	t.Setenv("ELASTICCLAW_GROK_CLI_VERSION", "9.9.9")

	binDir := filepath.Join(root, "model-clis", "grok", "9.9.9", "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0750); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(binDir, "grok")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0750); err != nil {
		t.Fatal(err)
	}

	cli, gotBinDir, err := ensureModelAuthCLI(context.Background(), "grok")
	if err != nil {
		t.Fatal(err)
	}
	if cli.BinaryName != binaryPath {
		t.Fatalf("BinaryName = %q, want managed binary %q", cli.BinaryName, binaryPath)
	}
	if gotBinDir != binDir {
		t.Fatalf("binDir = %q, want %q", gotBinDir, binDir)
	}
}

func TestModelAuthCLIInstallLockIsScopedByInstallDir(t *testing.T) {
	first := modelAuthCLIInstallLock("/tmp/elasticclaw/model-clis/codex/1")
	again := modelAuthCLIInstallLock("/tmp/elasticclaw/model-clis/codex/1")
	other := modelAuthCLIInstallLock("/tmp/elasticclaw/model-clis/grok/1")

	if first != again {
		t.Fatal("same install dir returned different locks")
	}
	if first == other {
		t.Fatal("different install dirs shared the same lock")
	}
}

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

func TestCaptureModelAuthOutputExtractsCodexOneTimeCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}
	output := `Follow these steps to sign in with ChatGPT using device code authorization:

1. Open this link in your browser and sign in to your account
   https://auth.openai.com/codex/device

2. Enter this one-time code (expires in 15 minutes)
   2VX0-20MIV

Device codes are a common phishing target. Never share this code.
`

	s.captureModelAuthOutput(job, strings.NewReader(output), make(chan struct{}, 1))

	if job.Code != "2VX0-20MIV" {
		t.Fatalf("Code = %q, want Codex one-time code", job.Code)
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

func TestCaptureModelAuthOutputExtractsStandaloneNineDigitCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Enter this authorization code:\n123 456 789\n"), make(chan struct{}, 1))

	if job.Code != "123456789" {
		t.Fatalf("Code = %q, want normalized 9 digit device code", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsUnlabeledNineDigitCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device\n987654321\n"), make(chan struct{}, 1))

	if job.Code != "987654321" {
		t.Fatalf("Code = %q, want unlabeled 9 digit device code", job.Code)
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
