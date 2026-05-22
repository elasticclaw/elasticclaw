package bootopt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAggregateTiming(t *testing.T) {
	runs := []TimingRun{
		{DurationMs: 100, Error: ""},
		{DurationMs: 200, Error: ""},
		{DurationMs: 300, Error: ""},
		{DurationMs: 400, Error: ""},
		{DurationMs: 500, Error: ""},
		{DurationMs: 0, Error: "failed"}, // should be excluded
	}

	mean, median, p95 := AggregateTiming(runs)

	if mean != 300 {
		t.Errorf("mean = %d, want 300", mean)
	}
	if median != 300 {
		t.Errorf("median = %d, want 300", median)
	}
	if p95 != 500 {
		t.Errorf("p95 = %d, want 500", p95)
	}
}

func TestAggregateTiming_Empty(t *testing.T) {
	mean, median, p95 := AggregateTiming([]TimingRun{})
	if mean != 0 || median != 0 || p95 != 0 {
		t.Error("expected zeros for empty input")
	}
}

func TestAggregateTiming_AllErrors(t *testing.T) {
	runs := []TimingRun{
		{Error: "fail1"},
		{Error: "fail2"},
	}
	mean, median, p95 := AggregateTiming(runs)
	if mean != 0 || median != 0 || p95 != 0 {
		t.Error("expected zeros when all runs error")
	}
}

func TestRunTiming_AllErrorsReturnsError(t *testing.T) {
	runner := NewTestRunner(t.TempDir(), "exit 1")
	runs, err := runner.RunTiming(t.Context(), 2)
	if err == nil {
		t.Fatal("expected error when all timing runs fail")
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
}

func TestRunVMBootTestReturnsCleanupError(t *testing.T) {
	const clawID = "550e8400-e29b-41d4-a716-446655440000"
	bin := filepath.Join(t.TempDir(), "elasticclaw")
	script := `#!/bin/sh
case "$1" in
create)
	echo "Created claw bootopt (id: 550e8400-e29b-41d4-a716-446655440000)"
	;;
list)
	printf '[{"id":"550e8400-e29b-41d4-a716-446655440000","status":"online"}]\n'
	;;
kill)
	echo "kill failed for $2" >&2
	exit 17
	;;
*)
	echo "unexpected command: $1" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}

	runner := NewVMTestRunnerWithConfig(bin, "", "base")
	result, err := runner.RunVMBootTest(t.Context())
	if err == nil {
		t.Fatal("expected cleanup error")
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if result.ClawID != clawID {
		t.Fatalf("claw ID = %q, want %q", result.ClawID, clawID)
	}
	if !strings.Contains(result.Error, "cleanup claw "+clawID) {
		t.Fatalf("result error %q missing claw cleanup context", result.Error)
	}
	if !strings.Contains(err.Error(), "kill failed for "+clawID) {
		t.Fatalf("error %q missing kill failure output", err.Error())
	}
}
