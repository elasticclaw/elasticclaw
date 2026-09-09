package hub

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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

func TestSelectDaytonaKeepaliveClawsIncludesOffline(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db")+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}

	// This fixture only needs claws rows; skip the tenant/task_run parents.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}

	statuses := []string{"connected", "offline", "error", "deleted", "idle"}
	for _, status := range statuses {
		if _, err := db.Exec(
			`INSERT INTO claws(id,tenant_id,name,template,status,provider,provider_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			"claw-"+status, "tenant", "claw-"+status, "base", status, "daytona", "sandbox-"+status, now(),
		); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}
	// A daytona claw without a sandbox id must never be petted.
	if _, err := db.Exec(
		`INSERT INTO claws(id,tenant_id,name,template,status,provider,provider_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"claw-nosandbox", "tenant", "claw-nosandbox", "base", "connected", "daytona", "", now(),
	); err != nil {
		t.Fatal(err)
	}

	claws, err := selectDaytonaKeepaliveClaws(db)
	if err != nil {
		t.Fatalf("selectDaytonaKeepaliveClaws: %v", err)
	}
	got := map[string]bool{}
	for _, c := range claws {
		got[c.providerID] = true
	}
	want := map[string]bool{"sandbox-connected": true, "sandbox-offline": true}
	if len(got) != len(want) {
		t.Fatalf("selected sandboxes = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("selected sandboxes = %v, want %v", got, want)
		}
	}
}
