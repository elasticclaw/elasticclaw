package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type gatewayLogExecutorStub struct {
	result  *types.ExecResult
	results []*types.ExecResult
	err     error
	args    [][]string
	timeout time.Duration
}

func (s *gatewayLogExecutorStub) ExecWithTimeout(_ context.Context, _ string, args []string, timeout time.Duration) (*types.ExecResult, error) {
	s.args = append(s.args, args)
	s.timeout = timeout
	if len(s.results) > 0 {
		result := s.results[0]
		s.results = s.results[1:]
		return result, s.err
	}
	return s.result, s.err
}

func TestCaptureGatewayLogWritesSecureTail(t *testing.T) {
	dataDir := t.TempDir()
	executor := &gatewayLogExecutorStub{results: []*types.ExecResult{{Stdout: "gateway stopped: out of memory\n"}, {Stdout: "bridge reconnect timed out\n"}}}
	if err := captureGatewayLogWithExecutor(context.Background(), executor, "claw-123", "sandbox-123", dataDir); err != nil {
		t.Fatalf("captureGatewayLogWithExecutor: %v", err)
	}
	// One element, no "bash -c" prefix: the Daytona provider joins cmdArgs and wraps
	// them in `bash -c '...'`, so a prefix here nests a second shell that eats the
	// arguments and silently tails nothing. The stub cannot catch that, so assert the
	// shape.
	if got, want := executor.args, [][]string{{gatewayLogCaptureCommand}, {bridgeLogCaptureCommand}}; len(got) != len(want) || got[0][0] != want[0][0] || got[1][0] != want[1][0] {
		t.Fatalf("exec args = %q, want %q", got, want)
	}
	if executor.timeout != gatewayLogCaptureTimeout {
		t.Fatalf("exec timeout = %s, want %s", executor.timeout, gatewayLogCaptureTimeout)
	}

	files, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "claw-123-*.log"))
	if err != nil || len(files) != 2 {
		t.Fatalf("capture files = %q, %v; want two", files, err)
	}

	// Each file must carry the content of its OWN source. Asserting only that
	// some file contains some expected line would stay green if the two captures
	// were swapped, and an on-call reader would blame the bridge for a gateway
	// stall.
	for suffix, want := range map[string]string{
		"-gateway.log": "gateway stopped: out of memory",
		"-bridge.log":  "bridge reconnect timed out",
	} {
		matches, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "claw-123-*"+suffix))
		if err != nil || len(matches) != 1 {
			t.Fatalf("%s files = %q, %v; want one", suffix, matches, err)
		}
		contents, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(contents), want) {
			t.Fatalf("%s contents = %q, want it to contain %q", suffix, contents, want)
		}
		info, err := os.Stat(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Fatalf("%s mode = %o, want %o", suffix, got, want)
		}
	}
}

func TestCaptureGatewayLogExecErrorLeavesNoFile(t *testing.T) {
	dataDir := t.TempDir()
	executor := &gatewayLogExecutorStub{err: errors.New("sandbox not found")}
	// A transport error must still surface: silently returning nil would make a
	// rotated API key indistinguishable from "the sandbox had no logs", and the
	// caller's one-line failure log would never fire.
	if err := captureGatewayLogWithExecutor(context.Background(), executor, "claw-123", "sandbox-123", dataDir); err == nil {
		t.Fatal("captureGatewayLogWithExecutor error = nil, want error")
	}
	files, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("capture files = %q, want none", files)
	}
}

func TestCaptureGatewayLogCapturesOtherFileWhenOneTailIsEmpty(t *testing.T) {
	dataDir := t.TempDir()
	executor := &gatewayLogExecutorStub{results: []*types.ExecResult{{ExitCode: 1}, {Stdout: "bridge was stuck\n"}}}
	if err := captureGatewayLogWithExecutor(context.Background(), executor, "claw-123", "sandbox-123", dataDir); err != nil {
		t.Fatalf("captureGatewayLogWithExecutor: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "*-bridge.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("bridge capture files = %q, %v; want one", files, err)
	}
	// Glob rather than deriving the gateway name from the bridge one: each capture
	// stamps its own timestamp, so a derived path never exists and the assertion
	// would pass even if a gateway file had been written.
	gatewayFiles, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "*-gateway.log"))
	if err != nil || len(gatewayFiles) != 0 {
		t.Fatalf("gateway capture files = %q, %v; want none", gatewayFiles, err)
	}
}

func TestCaptureGatewayLogSkipsEmptyAndFailedTail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *types.ExecResult
	}{
		{"missing log", &types.ExecResult{ExitCode: 1}},
		{"empty log", &types.ExecResult{Stdout: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			executor := &gatewayLogExecutorStub{result: tc.result}
			if err := captureGatewayLogWithExecutor(context.Background(), executor, "claw-123", "sandbox-123", dataDir); err != nil {
				t.Fatalf("captureGatewayLogWithExecutor: %v", err)
			}
			// An empty capture file would read as a successful post-mortem that
			// happens to be blank, hiding the fact that nothing was preserved.
			files, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "*.log"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("capture files = %q, want none", files)
			}
		})
	}
}

func TestCaptureGatewayLogHonorsSizeCap(t *testing.T) {
	dataDir := t.TempDir()
	contents := make([]byte, gatewayLogCaptureLimit+100)
	for i := range contents {
		contents[i] = 'x'
	}
	executor := &gatewayLogExecutorStub{result: &types.ExecResult{Stdout: string(contents)}}
	if err := captureGatewayLogWithExecutor(context.Background(), executor, "claw-123", "sandbox-123", dataDir); err != nil {
		t.Fatalf("captureGatewayLogWithExecutor: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(dataDir, "diagnostics", "*.log"))
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Size(), int64(gatewayLogCaptureLimit); got != want {
		t.Fatalf("capture size = %d, want %d", got, want)
	}
}
