package hub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type keepaliveExecutorStub struct {
	errors   []error
	calls    int
	timeouts []time.Duration
}

func (s *keepaliveExecutorStub) ExecWithTimeout(_ context.Context, _ string, _ []string, timeout time.Duration) (*types.ExecResult, error) {
	s.timeouts = append(s.timeouts, timeout)
	var err error
	if s.calls < len(s.errors) {
		err = s.errors[s.calls]
	}
	s.calls++
	if err != nil {
		return nil, err
	}
	return &types.ExecResult{}, nil
}

func TestPetDaytonaSandboxRetriesTransientTimeout(t *testing.T) {
	executor := &keepaliveExecutorStub{errors: []error{errors.New("Daytona error (status 408): request timeout: command execution timeout"), nil}}
	if err := petDaytonaSandboxWithRetry(context.Background(), executor, "sandbox-id", []time.Duration{0}); err != nil {
		t.Fatalf("petDaytonaSandboxWithRetry: %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("keepalive attempts = %d, want 2", executor.calls)
	}
	for _, timeout := range executor.timeouts {
		if timeout != 5*time.Second {
			t.Fatalf("keepalive command timeout = %s, want 5s", timeout)
		}
	}
}

func TestPetDaytonaSandboxDoesNotRetryPermanentFailure(t *testing.T) {
	wantErr := errors.New("resource not found")
	executor := &keepaliveExecutorStub{errors: []error{wantErr}}
	err := petDaytonaSandboxWithRetry(context.Background(), executor, "sandbox-id", []time.Duration{0, 0})
	if !errors.Is(err, wantErr) {
		t.Fatalf("petDaytonaSandboxWithRetry error = %v, want %v", err, wantErr)
	}
	if executor.calls != 1 {
		t.Fatalf("keepalive attempts = %d, want 1", executor.calls)
	}
}
