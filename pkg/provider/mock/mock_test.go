package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/provider"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLifecycle(t *testing.T) {
	p := New()
	ctx := context.Background()

	inst, err := p.Create(ctx, types.CreateRequest{Name: "claw-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ID == "" || inst.Name != "claw-1" {
		t.Fatalf("unexpected instance: %+v", inst)
	}

	st, err := p.Status(ctx, inst.ID)
	if err != nil || st != types.StatusRunning {
		t.Fatalf("Status after create = %v, %v; want running, nil", st, err)
	}

	if err := p.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, _ := p.Status(ctx, inst.ID); st != types.StatusStopped {
		t.Fatalf("Status after stop = %v; want stopped", st)
	}

	if err := p.Start(ctx, inst.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st, _ := p.Status(ctx, inst.ID); st != types.StatusRunning {
		t.Fatalf("Status after start = %v; want running", st)
	}

	if err := p.Destroy(ctx, inst.ID, false); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if st, _ := p.Status(ctx, inst.ID); st != types.StatusNotFound {
		t.Fatalf("Status after destroy = %v; want not_found", st)
	}
}

func TestProgrammableStatus(t *testing.T) {
	p := New()
	p.SetStatus("seeded", types.StatusUnhealthy)

	st, err := p.Status(context.Background(), "seeded")
	if err != nil || st != types.StatusUnhealthy {
		t.Fatalf("Status = %v, %v; want unhealthy, nil", st, err)
	}

	list, err := p.List(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != "seeded" {
		t.Fatalf("List = %+v, %v; want single seeded instance", list, err)
	}
}

func TestFailureInjection(t *testing.T) {
	p := New()
	boom := errors.New("boom")

	p.Fail(OpCreate, boom)
	if _, err := p.Create(context.Background(), types.CreateRequest{Name: "x"}); !errors.Is(err, boom) {
		t.Fatalf("Create with injected failure = %v; want boom", err)
	}
	p.Fail(OpCreate, nil)
	if _, err := p.Create(context.Background(), types.CreateRequest{Name: "x"}); err != nil {
		t.Fatalf("Create after clearing failure: %v", err)
	}

	p.FailOnce(OpList, boom)
	if _, err := p.List(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("List first call = %v; want boom", err)
	}
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List second call = %v; want nil (FailOnce consumed)", err)
	}
}

func TestContextCancellation(t *testing.T) {
	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Create(ctx, types.CreateRequest{Name: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create with cancelled ctx = %v; want context.Canceled", err)
	}
	if _, err := p.Status(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Status with cancelled ctx = %v; want context.Canceled", err)
	}
}

func TestExecAndConnectProgrammableResults(t *testing.T) {
	p := New()
	ctx := context.Background()
	inst, err := p.Create(ctx, types.CreateRequest{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}

	p.SetExecResult(types.ExecResult{ExitCode: 3, Stderr: "nope"})
	res, err := p.Exec(ctx, inst.ID, []string{"false"})
	if err != nil || res.ExitCode != 3 || res.Stderr != "nope" {
		t.Fatalf("Exec = %+v, %v; want programmed result", res, err)
	}

	p.SetConnectInfo(types.ConnectInfo{Web: "https://example.test"})
	ci, err := p.Connect(ctx, inst.ID)
	if err != nil || ci.Web != "https://example.test" {
		t.Fatalf("Connect = %+v, %v; want programmed info", ci, err)
	}

	if _, err := p.Exec(ctx, "missing", nil); err == nil {
		t.Fatal("Exec on missing instance should fail")
	}
	if _, err := p.Connect(ctx, "missing"); err == nil {
		t.Fatal("Connect on missing instance should fail")
	}
}

func TestCallRecording(t *testing.T) {
	p := New()
	ctx := context.Background()
	inst, _ := p.Create(ctx, types.CreateRequest{Name: "x"})
	_, _ = p.Status(ctx, inst.ID)
	_ = p.Stop(ctx, inst.ID)

	calls := p.Calls()
	want := []Op{OpCreate, OpStatus, OpStop}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v; want %d entries", calls, len(want))
	}
	for i, op := range want {
		if calls[i].Op != op {
			t.Fatalf("calls[%d].Op = %q; want %q", i, calls[i].Op, op)
		}
	}
}

func TestRegistryIntegration(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("mock", New())

	got, ok := reg.Get("mock")
	if !ok {
		t.Fatal("registry did not return mock provider")
	}
	if got.Info().Name != "mock" {
		t.Fatalf("Info().Name = %q; want mock", got.Info().Name)
	}
	infos := reg.ListWithInfo()
	if len(infos) != 1 || infos[0].Name != "mock" {
		t.Fatalf("ListWithInfo = %+v", infos)
	}
}

func TestConcurrentAccess(t *testing.T) {
	p := New()
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_, _ = p.Create(ctx, types.CreateRequest{Name: "c"})
		}
	}()
	for i := 0; i < 100; i++ {
		_, _ = p.List(ctx)
		p.SetStatus("seeded", types.StatusRunning)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent creates did not finish")
	}
}
