package bootopt

import (
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
