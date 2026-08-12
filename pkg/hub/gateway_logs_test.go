package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type gatewayLogExecutorStub struct {
	result  *types.ExecResult
	err     error
	args    []string
	timeout time.Duration
}

func (s *gatewayLogExecutorStub) ExecWithTimeout(_ context.Context, _ string, args []string, timeout time.Duration) (*types.ExecResult, error) {
	s.args = args
	s.timeout = timeout
	return s.result, s.err
}

func TestCaptureGatewayLogWritesSecureTail(t *testing.T) {
	dataDir := t.TempDir()
	executor := &gatewayLogExecutorStub{result: &types.ExecResult{Stdout: "gateway stopped: out of memory\n"}}
	if err := captureGatewayLogWithExecutor(context.Background(), executor, "claw-123", "sandbox-123", dataDir); err != nil {
		t.Fatalf("captureGatewayLogWithExecutor: %v", err)
	}
	if got, want := executor.args, []string{"bash", "-lc", "tail -c 262144 ~/.openclaw/gateway.log"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("exec args = %q, want %q", got, want)
	}
	if executor.timeout != gatewayLogCaptureTimeout {
		t.Fatalf("exec timeout = %s, want %s", executor.timeout, gatewayLogCaptureTimeout)
	}

	files, err := filepath.Glob(filepath.Join(dataDir, "diagnostics", "claw-123-*.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("capture files = %q, %v; want one", files, err)
	}
	contents, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "gateway stopped: out of memory\n"; got != want {
		t.Fatalf("capture contents = %q, want %q", got, want)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("capture mode = %o, want %o", got, want)
	}
}

func TestCaptureGatewayLogExecErrorLeavesNoFile(t *testing.T) {
	dataDir := t.TempDir()
	executor := &gatewayLogExecutorStub{err: errors.New("sandbox not found")}
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
