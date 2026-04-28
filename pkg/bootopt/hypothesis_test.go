package bootopt

import (
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	ctx := PromptContext{
		Iteration: 1,
		CurrentCode: map[string]string{
			"cmd/claw-bridge/main.go": "package main\n",
		},
		BaselineMeanMs:   5000,
		KnownBottlenecks: []string{"apt-get update slow"},
	}

	prompt := BuildPrompt(ctx)

	if !strings.Contains(prompt, "apt-get update slow") {
		t.Error("prompt should contain known bottlenecks")
	}
	if !strings.Contains(prompt, "cmd/claw-bridge/main.go") {
		t.Error("prompt should contain current code files")
	}
	if !strings.Contains(prompt, "## Output Format") {
		t.Error("prompt should contain output format section")
	}
	if !strings.Contains(prompt, "Unified diff") {
		t.Error("prompt should ask for unified diff")
	}
}

func TestParseHypothesis_Valid(t *testing.T) {
	input := "Here's my hypothesis:\n\n" +
		"```json\n" +
		"{\n" +
		"  \"description\": \"Parallelize apt and npm installs\",\n" +
		"  \"rationale\": \"apt and npm are independent, can run in parallel\",\n" +
		"  \"target_files\": [\"cmd/claw-bridge/main.go\"],\n" +
		"  \"diff\": \"--- a/cmd/claw-bridge/main.go\\n+++ b/cmd/claw-bridge/main.go\\n@@ -1 +1 @@\\n-old\\n+new\\n\",\n" +
		"  \"risk_level\": \"low\",\n" +
		"  \"expected_win\": \"3-5 seconds\"\n" +
		"}\n" +
		"```\n"

	h, err := ParseHypothesis(input)
	if err != nil {
		t.Fatalf("parse valid hypothesis: %v", err)
	}

	if h.Description != "Parallelize apt and npm installs" {
		t.Errorf("description mismatch: %q", h.Description)
	}
	if h.RiskLevel != "low" {
		t.Errorf("risk level mismatch: %q", h.RiskLevel)
	}
	if len(h.TargetFiles) != 1 || h.TargetFiles[0] != "cmd/claw-bridge/main.go" {
		t.Errorf("target files mismatch: %v", h.TargetFiles)
	}
	if !strings.Contains(h.Diff, "--- a/") {
		t.Error("diff should be unified format")
	}
}

func TestParseHypothesis_IgnoresLaterCodeBlocks(t *testing.T) {
	input := "```json\n" +
		"{\n" +
		"  \"description\": \"Parallelize apt and npm installs\",\n" +
		"  \"rationale\": \"apt and npm are independent, can run in parallel\",\n" +
		"  \"target_files\": [\"cmd/claw-bridge/main.go\"],\n" +
		"  \"diff\": \"--- a/cmd/claw-bridge/main.go\\n+++ b/cmd/claw-bridge/main.go\\n@@ -1 +1 @@\\n-old\\n+new\\n\",\n" +
		"  \"risk_level\": \"low\",\n" +
		"  \"expected_win\": \"3-5 seconds\"\n" +
		"}\n" +
		"```\n\n" +
		"Here's why:\n" +
		"```go\n" +
		"// example\n" +
		"```\n"

	h, err := ParseHypothesis(input)
	if err != nil {
		t.Fatalf("parse hypothesis with later code block: %v", err)
	}
	if h.Description != "Parallelize apt and npm installs" {
		t.Errorf("description mismatch: %q", h.Description)
	}
}

func TestParseHypothesis_FallsBackToRawJSONAfterNonJSONCodeBlock(t *testing.T) {
	input := "Example helper:\n" +
		"```go\n" +
		"func main() {}\n" +
		"```\n\n" +
		"Use this hypothesis:\n" +
		"{\n" +
		"  \"description\": \"Parallelize apt and npm installs\",\n" +
		"  \"rationale\": \"apt and npm are independent, can run in parallel\",\n" +
		"  \"target_files\": [\"cmd/claw-bridge/main.go\"],\n" +
		"  \"diff\": \"--- a/cmd/claw-bridge/main.go\\n+++ b/cmd/claw-bridge/main.go\\n@@ -1 +1 @@\\n-old\\n+new\\n\",\n" +
		"  \"risk_level\": \"low\",\n" +
		"  \"expected_win\": \"3-5 seconds\"\n" +
		"}\n"

	h, err := ParseHypothesis(input)
	if err != nil {
		t.Fatalf("parse hypothesis after non-JSON code block: %v", err)
	}
	if h.Description != "Parallelize apt and npm installs" {
		t.Errorf("description mismatch: %q", h.Description)
	}
}

func TestParseHypothesis_NoJSON(t *testing.T) {
	_, err := ParseHypothesis("no json here")
	if err == nil {
		t.Error("expected error for missing JSON")
	}
}

func TestParseHypothesis_InvalidJSON(t *testing.T) {
	input := "```json\n{not json}\n```"
	_, err := ParseHypothesis(input)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseHypothesis_MissingFields(t *testing.T) {
	input := "```json\n{}\n```"
	_, err := ParseHypothesis(input)
	if err == nil {
		t.Error("expected error for missing fields")
	}
}
