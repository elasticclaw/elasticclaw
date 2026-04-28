//go:build ignore
// +build ignore

// Manual mode for bootopt — reads hypotheses from stdin instead of LLM.
//
// Usage:
//
//	go run ./cmd/bootopt/manual.go \
//	  -repo ~/.openclaw/workspace/elasticclaw \
//	  -baseline-runs 3 \
//	  -timing-runs 3
//
// Then paste a JSON hypothesis at the prompt.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/bootopt"
)

func main() {
	var (
		repoRoot     = flag.String("repo", ".", "Path to repo root")
		baselineRuns = flag.Int("baseline-runs", 3, "Number of runs for baseline")
		timingRuns   = flag.Int("timing-runs", 3, "Number of runs per hypothesis")
		stateDir     = flag.String("state-dir", filepath.Join(os.TempDir(), "bootopt"), "State directory")
		sessionID    = flag.String("session", time.Now().Format("20060102-150405"), "Session ID")
	)
	flag.Parse()

	absRepo, err := filepath.Abs(*repoRoot)
	if err != nil {
		log.Fatalf("resolve repo: %v", err)
	}

	stateMgr := bootopt.NewStateManager(*stateDir)
	state, err := stateMgr.Load(*sessionID)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	patchApplier := bootopt.NewPatchApplier(absRepo)
	testRunner := bootopt.NewTestRunner(absRepo, "go test ./pkg/hub/")

	if err := ensureCleanGit(absRepo); err != nil {
		log.Fatalf("git not clean: %v", err)
	}

	// Baseline
	if state.BaselineMeanMs == 0 {
		log.Printf("=== Measuring baseline (%d runs) ===", *baselineRuns)
		runs, err := testRunner.RunTiming(context.Background(), *baselineRuns)
		if err != nil {
			log.Fatalf("baseline: %v", err)
		}
		mean, median, p95 := bootopt.AggregateTiming(runs)
		state.BaselineMeanMs = mean
		log.Printf("Baseline: mean=%dms median=%dms p95=%dms", mean, median, p95)
		stateMgr.Save(state)
	} else {
		log.Printf("Using cached baseline: %dms", state.BaselineMeanMs)
	}

	// Interactive loop
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("\n=== Paste hypothesis JSON (or 'quit') ===")
		fmt.Println("Format: {\"description\":\"...\",\"rationale\":\"...\",\"target_files\":[\"...\"],\"diff\":\"...\",\"risk_level\":\"low\",\"expected_win\":\"...\"}")
		fmt.Print("> ")

		input, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalf("read input: %v", err)
		}
		input = strings.TrimSpace(input)

		if input == "quit" || input == "exit" {
			break
		}

		hypothesis, err := bootopt.ParseHypothesis(input)
		if err != nil {
			fmt.Printf("Parse error: %v\n", err)
			continue
		}

		fmt.Printf("\nHypothesis: %s\n", hypothesis.Description)
		fmt.Printf("Expected: %s | Risk: %s\n", hypothesis.ExpectedWin, hypothesis.RiskLevel)
		fmt.Print("Apply? [y/N]: ")

		confirm, _ := reader.ReadString('\n')
		if strings.TrimSpace(confirm) != "y" {
			fmt.Println("Skipped.")
			continue
		}

		// Apply
		rollback, err := patchApplier.Apply(hypothesis.Diff)
		if err != nil {
			fmt.Printf("Apply error: %v\n", err)
			continue
		}

		// Build check
		if err := patchApplier.VerifyBuild(); err != nil {
			fmt.Printf("Build error: %v\n", err)
			rollback()
			continue
		}

		// Correctness
		fmt.Println("Running correctness test...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		corrErr := testRunner.RunCorrectness(ctx)
		cancel()

		if corrErr != nil {
			fmt.Printf("Correctness FAILED: %v\n", corrErr)
			rollback()
			continue
		}
		fmt.Println("Correctness PASSED")

		// Timing
		fmt.Printf("Running timing test (%d runs)...\n", *timingRuns)
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
		runs, err := testRunner.RunTiming(ctx, *timingRuns)
		cancel()

		if err != nil {
			fmt.Printf("Timing FAILED: %v\n", err)
			rollback()
			continue
		}

		mean, median, p95 := bootopt.AggregateTiming(runs)
		fmt.Printf("Timing: mean=%dms median=%dms p95=%dms (baseline: %dms)\n",
			mean, median, p95, state.BaselineMeanMs)

		if mean < state.BaselineMeanMs {
			saved := state.BaselineMeanMs - mean
			fmt.Printf("IMPROVEMENT! Saved %dms (%.1f%%)\n", saved, float64(saved)*100/float64(state.BaselineMeanMs))
			if err := patchApplier.Commit(fmt.Sprintf("bootopt: %s", hypothesis.Description)); err != nil {
				fmt.Printf("Commit FAILED: %v\n", err)
				rollback()
				continue
			}
			state.BaselineMeanMs = mean
			state.KeptChanges = append(state.KeptChanges, bootopt.KeptChange{
				Iteration:   state.CurrentIteration + 1,
				Description: hypothesis.Description,
				Diff:        hypothesis.Diff,
				MeanMs:      mean,
				SavedMs:     saved,
				CommittedAt: time.Now(),
			})
		} else {
			fmt.Printf("No improvement. Rolling back.\n")
			rollback()
		}

		state.CurrentIteration++
		stateMgr.Save(state)
		fmt.Printf("\n%s", state.Summary())
	}

	fmt.Println("\nFinal summary:")
	fmt.Println(state.Summary())
}

func ensureCleanGit(repoRoot string) error {
	cmd := exec.Command("git", "diff", "--quiet")
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("repo has uncommitted changes")
	}
	cmd = exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("repo has staged changes")
	}
	return nil
}
