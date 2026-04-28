package bootopt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Hypothesis represents a proposed optimization from the LLM.
type Hypothesis struct {
	Description string   `json:"description"`     // Human-readable summary
	Rationale   string   `json:"rationale"`       // Why this should improve boot time
	TargetFiles []string `json:"target_files"`    // Files to modify (relative paths)
	Diff        string   `json:"diff"`            // Unified diff format
	RiskLevel   string   `json:"risk_level"`      // low|medium|high
	ExpectedWin string   `json:"expected_win"`    // e.g. "5-10 seconds"
}

// HypothesisResult tracks the outcome of testing a hypothesis.
type HypothesisResult struct {
	Hypothesis     Hypothesis  `json:"hypothesis"`
	Iteration      int         `json:"iteration"`
	Correct        bool        `json:"correct"`        // Did 1-run test pass?
	CorrectnessErr string      `json:"correctness_err,omitempty"`
	TimingRuns     []TimingRun `json:"timing_runs"`    // 10 runs
	MeanMs         int64       `json:"mean_ms"`
	MedianMs       int64       `json:"median_ms"`
	P95Ms          int64       `json:"p95_ms"`
	BaselineMeanMs int64       `json:"baseline_mean_ms"`
	Kept           bool        `json:"kept"`           // Did we keep this change?
	Reason         string      `json:"reason"`         // Why kept or discarded
}

// TimingRun is a single timed execution.
type TimingRun struct {
	StartMs    int64             `json:"start_ms"`     // Unix millis
	DurationMs int64             `json:"duration_ms"`
	PhaseTimes map[string]int64  `json:"phase_times"`  // per-phase breakdown
	Error      string            `json:"error,omitempty"`
}

// PromptContext holds the current codebase state for LLM prompting.
type PromptContext struct {
	Iteration       int                `json:"iteration"`
	PreviousResults []HypothesisResult `json:"previous_results"`
	CurrentCode     map[string]string  `json:"current_code"`  // file path → content
	BaselineMeanMs  int64              `json:"baseline_mean_ms"`
	KnownBottlenecks []string          `json:"known_bottlenecks"`
}

// BuildPrompt constructs the full prompt for hypothesis generation.
func BuildPrompt(ctx PromptContext) string {
	var b strings.Builder

	b.WriteString(`You are an expert systems engineer optimizing VM bootstrap time for an AI agent provisioning system.

## System Overview

ElasticClaw provisions ephemeral VMs that run AI agents. The bootstrap sequence is:
1. VM starts (sandbox time — we can't control this)
2. SSH connects, runs bootstrap script
3. Bootstrap script downloads claw-bridge binary
4. claw-bridge --bootstrap runs:
   a. Install Node.js 24 + git via apt
   b. Install Nix in background (if enabled)
   c. npm install -g openclaw@latest
   d. openclaw onboard (first-run config)
   e. Start gateway, stop it (first-run writes device.json)
   f. Patch openclaw.json with model config, auth, disable bonjour
   g. Restart gateway, wait for health
   h. Wait for device.json
   i. Write bootstrap completion file
   j. Continue to bridge connect loop
5. Bridge connects to hub via WebSocket
6. Hub sends intro message, agent is live

## Your Task

Generate ONE concrete optimization hypothesis. Focus on steps we control (3-4, NOT sandbox startup).

Current known bottlenecks:
`)
	for _, bneck := range ctx.KnownBottlenecks {
		fmt.Fprintf(&b, "- %s\n", bneck)
	}

	if len(ctx.PreviousResults) > 0 {
		b.WriteString("\n## Previous Iterations\n\n")
		for _, r := range ctx.PreviousResults {
			status := "DISCARDED"
			if r.Kept {
				status = "KEPT"
			}
			fmt.Fprintf(&b, "Iteration %d: %s (%s)\n", r.Iteration, r.Hypothesis.Description, status)
			fmt.Fprintf(&b, "  Expected: %s | Actual mean: %dms (baseline: %dms)\n", r.Hypothesis.ExpectedWin, r.MeanMs, r.BaselineMeanMs)
			if r.CorrectnessErr != "" {
				fmt.Fprintf(&b, "  Correctness error: %s\n", r.CorrectnessErr)
			}
			fmt.Fprintf(&b, "  Reason: %s\n\n", r.Reason)
		}
	}

	b.WriteString("\n## Current Code (key files)\n\n")
	for path, content := range ctx.CurrentCode {
		fmt.Fprintf(&b, "### %s\n```go\n%s\n```\n\n", path, content)
	}

	b.WriteString("## Output Format\n\n")
	b.WriteString("Respond with a JSON object:\n\n")
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString(`  "description": "Short title of the optimization",` + "\n")
	b.WriteString(`  "rationale": "Detailed explanation of why this improves boot time",` + "\n")
	b.WriteString(`  "target_files": ["relative/path/to/file.go"],` + "\n")
	b.WriteString(`  "diff": "Unified diff (--- old/... +++ new/... format)",` + "\n")
	b.WriteString(`  "risk_level": "low|medium|high",` + "\n")
	b.WriteString(`  "expected_win": "e.g. 3-5 seconds"` + "\n")
	b.WriteString("}\n")
	b.WriteString("```\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- ONLY modify files in the bootstrap/claw-bridge path\n")
	b.WriteString("- NEVER remove safety checks (error handling, health checks) entirely — optimize around them\n")
	b.WriteString("- NEVER break the build (go build ./... must pass)\n")
	b.WriteString("- NEVER change external APIs or hub protocol\n")
	b.WriteString("- Prefer parallelization over removal\n")
	b.WriteString("- Prefer caching/pre-installation over skipping\n")
	b.WriteString("- If a step seems skippable, explain why it's safe\n")
	b.WriteString("- Diff must apply cleanly with 'git apply'\n")

	return b.String()
}

// ParseHypothesis extracts a Hypothesis from LLM response text.
func ParseHypothesis(text string) (*Hypothesis, error) {
	// Try to find JSON block
	start := strings.Index(text, "```json")
	if start == -1 {
		start = strings.Index(text, "```")
	}
	if start == -1 {
		// Try raw JSON
		start = strings.Index(text, "{")
	}
	if start == -1 {
		return nil, fmt.Errorf("no JSON found in response")
	}

	// Skip ```json marker
	if strings.HasPrefix(text[start:], "```json") {
		start += 7
	} else if strings.HasPrefix(text[start:], "```") {
		start += 3
	}

	endOffset := strings.Index(text[start:], "```")
	end := len(text)
	if endOffset != -1 {
		end = start + endOffset
	}
	if end <= start {
		end = len(text)
	}

	jsonStr := strings.TrimSpace(text[start:end])

	var h Hypothesis
	if err := json.Unmarshal([]byte(jsonStr), &h); err != nil {
		return nil, fmt.Errorf("parse hypothesis JSON: %w", err)
	}

	// Validate
	if h.Description == "" {
		return nil, fmt.Errorf("hypothesis missing description")
	}
	if h.Diff == "" {
		return nil, fmt.Errorf("hypothesis missing diff")
	}
	if len(h.TargetFiles) == 0 {
		return nil, fmt.Errorf("hypothesis missing target_files")
	}

	return &h, nil
}
