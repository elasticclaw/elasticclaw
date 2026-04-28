package main

import (
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
		iterations    = flag.Int("iterations", 10, "Number of optimization iterations")
		anthropicKey  = flag.String("anthropic-key", os.Getenv("ANTHROPIC_API_KEY"), "Anthropic API key")
		testCommand   = flag.String("test-command", "go test ./pkg/hub/", "Test command to run")
		repoRoot      = flag.String("repo", ".", "Path to repo root")
		stateDir      = flag.String("state-dir", filepath.Join(os.TempDir(), "bootopt"), "State persistence directory")
		sessionID     = flag.String("session", time.Now().Format("20060102-150405"), "Session ID")
		baselineRuns  = flag.Int("baseline-runs", 10, "Number of runs for baseline measurement")
		timingRuns    = flag.Int("timing-runs", 10, "Number of proxy runs per hypothesis (only without -vm-tests)")
		keyFiles      = flag.String("key-files", "cmd/claw-bridge/main.go,pkg/hub/bootstrap.go,pkg/install/scripts.go", "Comma-separated key files for LLM context")
		useVMTests    = flag.Bool("vm-tests", false, "Use real Replicated VM tests for timing (slower but accurate)")
		vmTestRuns    = flag.Int("vm-test-runs", 3, "Number of VM tests per hypothesis (only with -vm-tests)")
	)
	flag.Parse()

	if *anthropicKey == "" {
		log.Fatal("Anthropic API key required (set ANTHROPIC_API_KEY or use -anthropic-key)")
	}

	absRepo, err := filepath.Abs(*repoRoot)
	if err != nil {
		log.Fatalf("resolve repo path: %v", err)
	}

	// Initialize state
	stateMgr := bootopt.NewStateManager(*stateDir)
	state, err := stateMgr.Load(*sessionID)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// Initialize components
	patchApplier := bootopt.NewPatchApplier(absRepo)
	testRunner := bootopt.NewTestRunner(absRepo, *testCommand)
	llmClient := bootopt.NewAnthropicClient(*anthropicKey)

	// Ensure clean git state
	if err := ensureCleanGit(absRepo); err != nil {
		log.Fatalf("git not clean: %v", err)
	}

	// Measure baseline if not already done
	if state.BaselineMeanMs == 0 {
		if *useVMTests {
			log.Printf("=== Measuring VM baseline (%d runs) ===", *vmTestRuns)
			vmRunner := bootopt.NewVMTestRunner()
			var vmResults []*bootopt.VMBootResult
			for j := 0; j < *vmTestRuns; j++ {
				log.Printf("VM baseline run %d/%d...", j+1, *vmTestRuns)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				res, err := vmRunner.RunVMBootTest(ctx)
				cancel()
				if err != nil {
					log.Printf("VM baseline run %d failed: %v", j+1, err)
					continue
				}
				vmResults = append(vmResults, res)
				log.Printf("VM run %d: total=%dms phases=%v", j+1, res.TotalMs, res.Phases)
			}
			mean, median, p95, phaseMeans := bootopt.AggregateVMBootResults(vmResults)
			state.BaselineMeanMs = mean
			log.Printf("VM Baseline: mean=%dms median=%dms p95=%dms", mean, median, p95)
			for phase, ms := range phaseMeans {
				log.Printf("  Phase %s: %dms", phase, ms)
			}
		} else {
			log.Printf("=== Measuring proxy baseline (%d runs) ===", *baselineRuns)
			baselineRuns, err := testRunner.RunTiming(context.Background(), *baselineRuns)
			if err != nil {
				log.Fatalf("baseline timing failed: %v", err)
			}
			mean, median, p95 := bootopt.AggregateTiming(baselineRuns)
			state.BaselineMeanMs = mean
			log.Printf("Proxy Baseline: mean=%dms median=%dms p95=%dms", mean, median, p95)
		}
		if err := stateMgr.Save(state); err != nil {
			log.Printf("warning: save state: %v", err)
		}
	} else {
		log.Printf("=== Using cached baseline: %dms ===", state.BaselineMeanMs)
	}

	// Main optimization loop
	keyFileList := strings.Split(*keyFiles, ",")
	for i := state.CurrentIteration; i < *iterations; i++ {
		log.Printf("\n=== Iteration %d/%d ===", i+1, *iterations)
		state.CurrentIteration = i + 1

		// Gather current code context
		currentCode, err := patchApplier.GetCurrentCode(keyFileList)
		if err != nil {
			log.Printf("ERROR: get current code: %v", err)
			continue
		}

		// Build prompt
		promptCtx := bootopt.PromptContext{
			Iteration:          i + 1,
			PreviousResults:    state.Results,
			CurrentCode:        currentCode,
			BaselineMeanMs:     state.BaselineMeanMs,
			KnownBottlenecks:   getKnownBottlenecks(),
		}
		prompt := bootopt.BuildPrompt(promptCtx)

		// Generate hypothesis
		log.Printf("Generating hypothesis...")
		resp, err := llmClient.GenerateHypothesis(context.Background(), prompt)
		if err != nil {
			log.Printf("ERROR: generate hypothesis: %v", err)
			continue
		}

		hypothesis, err := bootopt.ParseHypothesis(resp)
		if err != nil {
			log.Printf("ERROR: parse hypothesis: %v", err)
			continue
		}

		log.Printf("Hypothesis: %s", hypothesis.Description)
		log.Printf("Expected win: %s", hypothesis.ExpectedWin)
		log.Printf("Risk: %s", hypothesis.RiskLevel)

		// Apply patch
		rollback, err := patchApplier.Apply(hypothesis.Diff)
		if err != nil {
			log.Printf("ERROR: apply patch: %v", err)
			result := bootopt.HypothesisResult{
				Hypothesis:     *hypothesis,
				Iteration:      i + 1,
				Correct:        false,
				CorrectnessErr: fmt.Sprintf("patch apply: %v", err),
				Kept:           false,
				Reason:         "Patch did not apply cleanly",
			}
			state.Results = append(state.Results, result)
			continue
		}

		// Verify build
		if err := patchApplier.VerifyBuild(); err != nil {
			log.Printf("ERROR: build failed: %v", err)
			rollback()
			result := bootopt.HypothesisResult{
				Hypothesis:     *hypothesis,
				Iteration:      i + 1,
				Correct:        false,
				CorrectnessErr: fmt.Sprintf("build: %v", err),
				Kept:           false,
				Reason:         "Build failed after patch",
			}
			state.Results = append(state.Results, result)
			continue
		}

		// Correctness test (1 run)
		log.Printf("Running correctness test...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		correctnessErr := testRunner.RunCorrectness(ctx)
		cancel()

		if correctnessErr != nil {
			log.Printf("ERROR: correctness test failed: %v", correctnessErr)
			rollback()
			result := bootopt.HypothesisResult{
				Hypothesis:     *hypothesis,
				Iteration:      i + 1,
				Correct:        false,
				CorrectnessErr: correctnessErr.Error(),
				Kept:           false,
				Reason:         "Correctness test failed",
			}
			state.Results = append(state.Results, result)
			continue
		}

		log.Printf("Correctness test passed!")

		// Performance test
		var mean, median, p95 int64

		if *useVMTests {
			log.Printf("Running VM timing test (%d runs)...", *vmTestRuns)
			vmRunner := bootopt.NewVMTestRunner()
			var vmResults []*bootopt.VMBootResult
			for j := 0; j < *vmTestRuns; j++ {
				log.Printf("VM test run %d/%d...", j+1, *vmTestRuns)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				res, err := vmRunner.RunVMBootTest(ctx)
				cancel()
				if err != nil {
					log.Printf("VM test run %d failed: %v", j+1, err)
					continue
				}
				vmResults = append(vmResults, res)
				log.Printf("VM run %d: total=%dms phases=%v", j+1, res.TotalMs, res.Phases)
			}
			if len(vmResults) == 0 {
				log.Printf("ERROR: all VM tests failed")
				rollback()
				result := bootopt.HypothesisResult{
					Hypothesis:     *hypothesis,
					Iteration:      i + 1,
					Correct:        true,
					Kept:           false,
					Reason:         "All VM tests failed",
				}
				state.Results = append(state.Results, result)
				continue
			}
			mean, median, p95, _ = bootopt.AggregateVMBootResults(vmResults)
			log.Printf("VM Timing: mean=%dms median=%dms p95=%dms (baseline: %dms)",
				mean, median, p95, state.BaselineMeanMs)
		} else {
			log.Printf("Running proxy timing test (%d runs)...", *timingRuns)
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
			proxyRuns, err := testRunner.RunTiming(ctx, *timingRuns)
			cancel()

			if err != nil {
				log.Printf("ERROR: timing test failed: %v", err)
				rollback()
				result := bootopt.HypothesisResult{
					Hypothesis:     *hypothesis,
					Iteration:      i + 1,
					Correct:        true,
					CorrectnessErr: "",
					Kept:           false,
					Reason:         fmt.Sprintf("Timing test failed: %v", err),
				}
				state.Results = append(state.Results, result)
				continue
			}

			mean, median, p95 = bootopt.AggregateTiming(proxyRuns)
			log.Printf("Proxy Timing: mean=%dms median=%dms p95=%dms (baseline: %dms)",
				mean, median, p95, state.BaselineMeanMs)
		}

		// Decision
		result := bootopt.HypothesisResult{
			Hypothesis:     *hypothesis,
			Iteration:      i + 1,
			Correct:        true,
			MeanMs:         mean,
			MedianMs:       median,
			P95Ms:          p95,
			BaselineMeanMs: state.BaselineMeanMs,
		}

		if mean < state.BaselineMeanMs {
			// Improvement! Keep it.
			saved := state.BaselineMeanMs - mean
			result.Kept = true
			result.Reason = fmt.Sprintf("Faster by %dms (%.1f%%)", saved, float64(saved)*100/float64(state.BaselineMeanMs))
			state.KeptChanges = append(state.KeptChanges, bootopt.KeptChange{
				Iteration:   i + 1,
				Description: hypothesis.Description,
				Diff:        hypothesis.Diff,
				MeanMs:      mean,
				SavedMs:     saved,
				CommittedAt: time.Now(),
			})
			// Update baseline to new, faster baseline
			state.BaselineMeanMs = mean
			log.Printf("KEPT: %s", result.Reason)

			// Commit
			commitMsg := fmt.Sprintf("bootopt(iter-%d): %s\n\n%s\n\nSaved: %dms (%.1f%%)",
				i+1, hypothesis.Description, hypothesis.Rationale, saved,
				float64(saved)*100/float64(result.BaselineMeanMs))
			if err := patchApplier.Commit(commitMsg); err != nil {
				log.Printf("warning: commit failed: %v", err)
			}
		} else {
			// No improvement — rollback
			result.Kept = false
			result.Reason = fmt.Sprintf("No improvement (mean %dms >= baseline %dms)", mean, state.BaselineMeanMs)
			log.Printf("DISCARDED: %s", result.Reason)
			rollback()
		}

		state.Results = append(state.Results, result)

		// Save state after each iteration
		if err := stateMgr.Save(state); err != nil {
			log.Printf("warning: save state: %v", err)
		}

		log.Printf("\n%s", state.Summary())
	}

	log.Printf("\n=== Final Summary ===")
	log.Printf("%s", state.Summary())
	for _, k := range state.KeptChanges {
		log.Printf("  Iteration %d: %s (saved %dms)", k.Iteration, k.Description, k.SavedMs)
	}
}

func ensureCleanGit(repoRoot string) error {
	cmd := exec.Command("git", "diff", "--quiet")
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("repo has uncommitted changes")
	}
	return nil
}

func getKnownBottlenecks() []string {
	return []string{
		"apt-get update takes 5-10s",
		"Node.js install via apt requires GPG key + source list setup",
		"npm install -g openclaw@latest downloads ~50MB",
		"openclaw onboard runs interactive setup even with --non-interactive",
		"gateway started twice (first for device.json, then restarted with config)",
		"device.json wait polls every 1s for up to 120s",
		"Nix install is in background but blocks nothing — already off critical path",
		"claw-bridge binary download from GitHub releases ~10MB",
		"No pre-baked VM images — every boot starts from fresh Ubuntu",
	}
}


