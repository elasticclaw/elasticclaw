package bootopt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// TestRunner executes correctness and performance tests.
type TestRunner struct {
	RepoRoot      string
	TestCommand   string   // e.g. "make test-bootstrap"
	ContainerImage string  // e.g. "ubuntu:24.04" for container tests
}

// NewTestRunner creates a test runner.
func NewTestRunner(repoRoot, testCommand string) *TestRunner {
	return &TestRunner{
		RepoRoot:    repoRoot,
		TestCommand: testCommand,
	}
}

// RunCorrectness runs the test suite once to verify the change works.
func (tr *TestRunner) RunCorrectness(ctx context.Context) error {
	// Run Go tests for bootstrap
	cmd := exec.CommandContext(ctx, "go", "test", "-v", "-run", "Bootstrap", "./pkg/hub/")
	cmd.Dir = tr.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootstrap tests failed: %w\n%s", err, string(out))
	}

	// Run shellcheck tests
	cmd = exec.CommandContext(ctx, "go", "test", "-v", "-run", "Shellcheck", "./pkg/hub/")
	cmd.Dir = tr.RepoRoot
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("shellcheck tests failed: %w\n%s", err, string(out))
	}

	// Run stdin-exec tests (catches heredoc bugs)
	cmd = exec.CommandContext(ctx, "go", "test", "-v", "-run", "StdinExec", "./pkg/hub/")
	cmd.Dir = tr.RepoRoot
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("stdin exec tests failed: %w\n%s", err, string(out))
	}

	// Run full build
	cmd = exec.CommandContext(ctx, "go", "build", "./...")
	cmd.Dir = tr.RepoRoot
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build failed: %w\n%s", err, string(out))
	}

	return nil
}

// RunTiming performs N timing runs and returns aggregated results.
func (tr *TestRunner) RunTiming(ctx context.Context, runs int) ([]TimingRun, error) {
	results := make([]TimingRun, 0, runs)

	for i := 0; i < runs; i++ {
		run, err := tr.runSingleTiming(ctx)
		if err != nil {
			results = append(results, TimingRun{
				StartMs: time.Now().UnixMilli(),
				Error:   err.Error(),
			})
			continue
		}
		results = append(results, run)
	}

	return results, nil
}

// runSingleTiming measures bootstrap script generation + containerized execution.
func (tr *TestRunner) runSingleTiming(ctx context.Context) (TimingRun, error) {
	start := time.Now()
	phaseTimes := make(map[string]int64)

	// Phase 1: Script generation (pure Go)
	phaseStart := time.Now()
	cmd := exec.CommandContext(ctx, "go", "test", "-run", "TestBootstrapScript_ContainsBootstrapMode", "./pkg/hub/")
	cmd.Dir = tr.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return TimingRun{}, fmt.Errorf("script generation test failed: %w\n%s", err, string(out))
	}
	phaseTimes["script_generation"] = time.Since(phaseStart).Milliseconds()

	// Phase 2: Shellcheck validation
	phaseStart = time.Now()
	cmd = exec.CommandContext(ctx, "go", "test", "-run", "TestBootstrapScript_Shellcheck", "./pkg/hub/")
	cmd.Dir = tr.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		// Shellcheck might not be installed — skip timing if so
		if strings.Contains(string(out), "shellcheck not in PATH") {
			phaseTimes["shellcheck"] = -1 // skipped
		} else {
			return TimingRun{}, fmt.Errorf("shellcheck test failed: %w\n%s", err, string(out))
		}
	} else {
		phaseTimes["shellcheck"] = time.Since(phaseStart).Milliseconds()
	}

	// Phase 3: Stdin parse test (catches heredoc bugs)
	phaseStart = time.Now()
	cmd = exec.CommandContext(ctx, "go", "test", "-run", "TestBootstrapScript_StdinExec", "./pkg/hub/")
	cmd.Dir = tr.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return TimingRun{}, fmt.Errorf("stdin exec test failed: %w\n%s", err, string(out))
	}
	phaseTimes["stdin_parse"] = time.Since(phaseStart).Milliseconds()

	// Phase 4: Build Go binaries (hub + bridge)
	phaseStart = time.Now()
	cmd = exec.CommandContext(ctx, "go", "build", "./cmd/claw-bridge")
	cmd.Dir = tr.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return TimingRun{}, fmt.Errorf("build claw-bridge failed: %w\n%s", err, string(out))
	}
	phaseTimes["build_bridge"] = time.Since(phaseStart).Milliseconds()

	phaseStart = time.Now()
	cmd = exec.CommandContext(ctx, "go", "build", "-o", "/tmp/elasticclaw-test", ".")
	cmd.Dir = tr.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return TimingRun{}, fmt.Errorf("build hub binary failed: %w\n%s", err, string(out))
	}
	phaseTimes["build_hub"] = time.Since(phaseStart).Milliseconds()

	// Phase 5: Container test (if available)
	if tr.ContainerImage != "" && os.Getenv("ELASTICCLAW_CONTAINER_TESTS") != "" {
		phaseStart = time.Now()
		cmd = exec.CommandContext(ctx, "go", "test", "-run", "TestBootstrapScript_ContainerRun", "./pkg/hub/")
		cmd.Dir = tr.RepoRoot
		cmd.Env = append(os.Environ(), "ELASTICCLAW_CONTAINER_TESTS=1")
		// Container test is optional — don't fail if skipped
		cmd.CombinedOutput()
		phaseTimes["container_test"] = time.Since(phaseStart).Milliseconds()
	}

	return TimingRun{
		StartMs:    start.UnixMilli(),
		DurationMs: time.Since(start).Milliseconds(),
		PhaseTimes: phaseTimes,
	}, nil
}

// AggregateTiming computes mean, median, p95 from timing runs.
func AggregateTiming(runs []TimingRun) (mean, median, p95 int64) {
	var valid []int64
	for _, r := range runs {
		if r.Error == "" {
			valid = append(valid, r.DurationMs)
		}
	}
	if len(valid) == 0 {
		return 0, 0, 0
	}

	// Mean
	var sum int64
	for _, v := range valid {
		sum += v
	}
	mean = sum / int64(len(valid))

	// Median
	sort.Slice(valid, func(i, j int) bool { return valid[i] < valid[j] })
	mid := len(valid) / 2
	if len(valid)%2 == 0 {
		median = (valid[mid-1] + valid[mid]) / 2
	} else {
		median = valid[mid]
	}

	// P95
	p95Idx := int(float64(len(valid)) * 0.95)
	if p95Idx >= len(valid) {
		p95Idx = len(valid) - 1
	}
	p95 = valid[p95Idx]

	return mean, median, p95
}
