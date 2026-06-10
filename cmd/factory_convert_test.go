package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFactoryConvertCLIWritesWorkflowAndPreservesLegacyFactory(t *testing.T) {
	withTempWorkingDir(t)
	writeFactoryConvertCLIWorkspaceConfig(t, "engineering", "workspace_secret")
	factoryPath, pipelinePath := writeFactoryConvertCLIFactory(t, "issue-triage", `
name: issue-triage
integration: github-issues
template: engineering
trigger_status: agent-ready
trigger_repos:
  - owner/repo
labels:
  - bug
secret_refs:
  WORKFLOW_SECRET: workspace_secret
`, validFactoryConvertCLIPipeline("inject: start"))
	beforeFactory := readFactoryConvertCLIFile(t, factoryPath)
	beforePipeline := readFactoryConvertCLIFile(t, pipelinePath)

	out, err := executeFactoryCommand(t, "convert", "issue-triage", "--workspace", "engineering")
	if err != nil {
		t.Fatalf("factory convert: %v\n%s", err, out)
	}

	workflowPath := filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "issue-triage.yaml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read converted workflow: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{
		"name: issue-triage",
		"enabled: false",
		"integration: github-issues",
		"github_issues:",
		"owner/repo",
		"agent-ready",
		"pipeline_yaml: |",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("converted workflow missing %q:\n%s", want, rendered)
		}
	}
	if !strings.Contains(out, workflowPath) {
		t.Fatalf("output %q did not mention path %q", out, workflowPath)
	}
	if got := readFactoryConvertCLIFile(t, factoryPath); got != beforeFactory {
		t.Fatalf("factory.yaml was mutated:\nbefore:\n%s\nafter:\n%s", beforeFactory, got)
	}
	if got := readFactoryConvertCLIFile(t, pipelinePath); got != beforePipeline {
		t.Fatalf("pipeline.yaml was mutated:\nbefore:\n%s\nafter:\n%s", beforePipeline, got)
	}
}

func TestFactoryConvertCLIDoesNotWriteWorkflowWhenBlocked(t *testing.T) {
	withTempWorkingDir(t)
	writeFactoryConvertCLIWorkspaceConfig(t, "engineering", "workspace_secret")
	factoryPath, pipelinePath := writeFactoryConvertCLIFactory(t, "github-pr", `
name: github-pr
integration: github
template: engineering
trigger:
  on: pull_request
  action: opened
`, validFactoryConvertCLIPipeline("inject: start"))
	beforeFactory := readFactoryConvertCLIFile(t, factoryPath)
	beforePipeline := readFactoryConvertCLIFile(t, pipelinePath)

	oldJSONOut := jsonOut
	jsonOut = true
	t.Cleanup(func() {
		jsonOut = oldJSONOut
	})

	out, err := executeFactoryCommand(t, "convert", "github-pr", "--workspace", "engineering")
	if err == nil {
		t.Fatalf("factory convert error = nil, want critical diagnostic")
	}

	var result factoryConvertResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode JSON result: %v\n%s", err, out)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if result.Summary.Critical == 0 {
		t.Fatalf("critical diagnostics = 0, want critical: %#v", result.Diagnostics)
	}
	if _, statErr := os.Stat(filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "github-pr.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("blocked conversion wrote workflow, stat err = %v", statErr)
	}
	if got := readFactoryConvertCLIFile(t, factoryPath); got != beforeFactory {
		t.Fatalf("factory.yaml was mutated:\nbefore:\n%s\nafter:\n%s", beforeFactory, got)
	}
	if got := readFactoryConvertCLIFile(t, pipelinePath); got != beforePipeline {
		t.Fatalf("pipeline.yaml was mutated:\nbefore:\n%s\nafter:\n%s", beforePipeline, got)
	}
}

func TestFactoryConvertCLIHonorsOutputPath(t *testing.T) {
	withTempWorkingDir(t)
	writeFactoryConvertCLIWorkspaceConfig(t, "engineering", "workspace_secret")
	writeFactoryConvertCLIFactory(t, "linear-triage", `
name: linear-triage
integration: linear
template: engineering
workspace: product
trigger_status: Ready for Agent
`, validFactoryConvertCLIPipeline("move_issue: In Progress"))

	customPath := filepath.Join("converted", "linear.yaml")
	out, err := executeFactoryCommand(t, "convert", "linear-triage", "--workspace", "engineering", "--output", customPath)
	if err != nil {
		t.Fatalf("factory convert: %v\n%s", err, out)
	}
	if _, err := os.Stat(customPath); err != nil {
		t.Fatalf("custom output path was not written: %v", err)
	}
	defaultPath := filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "linear-triage.yaml")
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("default output path exists unexpectedly, stat err = %v", err)
	}
}

func TestFactoryConvertCLIReportsInputFactoryNameWhenWorkflowNameDiffers(t *testing.T) {
	withTempWorkingDir(t)
	writeFactoryConvertCLIWorkspaceConfig(t, "engineering", "workspace_secret")
	writeFactoryConvertCLIFactory(t, "legacy-issue-triage", `
name: issue-triage
integration: github-issues
template: engineering
trigger_status: agent-ready
trigger_repos:
  - owner/repo
secret_refs:
  WORKFLOW_SECRET: workspace_secret
`, validFactoryConvertCLIPipeline("inject: start"))

	out, err := executeFactoryCommand(t, "convert", "legacy-issue-triage", "--workspace", "engineering")
	if err != nil {
		t.Fatalf("factory convert: %v\n%s", err, out)
	}

	if !strings.Contains(out, `Converted factory "legacy-issue-triage" to workflow "issue-triage"`) {
		t.Fatalf("output %q did not report distinct factory and workflow names", out)
	}
}

func TestFactoryConvertCLIBlocksWhenWorkspaceDoesNotHaveLegacyTemplateFile(t *testing.T) {
	withTempWorkingDir(t)
	writeFactoryConvertCLIWorkspaceConfig(t, "engineering", "workspace_secret")
	factoryPath, pipelinePath := writeFactoryConvertCLIFactory(t, "linear-triage", `
name: linear-triage
integration: linear
template: engineering
workspace: product
trigger_status: Ready for Agent
`, validFactoryConvertCLIPipeline("move_issue: In Progress"))
	extraPath := writeFactoryConvertCLIFile(t, filepath.Join(".elasticclaw", "factories", "linear-triage", "BOOTSTRAP.md"), "legacy bootstrap\n")
	beforeFactory := readFactoryConvertCLIFile(t, factoryPath)
	beforePipeline := readFactoryConvertCLIFile(t, pipelinePath)
	beforeExtra := readFactoryConvertCLIFile(t, extraPath)

	oldJSONOut := jsonOut
	jsonOut = true
	t.Cleanup(func() {
		jsonOut = oldJSONOut
	})

	out, err := executeFactoryCommand(t, "convert", "linear-triage", "--workspace", "engineering")
	if err == nil {
		t.Fatalf("factory convert error = nil, want template file parity failure")
	}
	var result factoryConvertResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode JSON result: %v\n%s", err, out)
	}
	if result.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", result.Status)
	}
	if !factoryConvertResultHasDiagnostic(result, "factory-convert-template-file-missing") {
		t.Fatalf("missing template file diagnostic in %#v", result.Diagnostics)
	}
	if _, statErr := os.Stat(filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "linear-triage.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("blocked conversion wrote workflow, stat err = %v", statErr)
	}
	if got := readFactoryConvertCLIFile(t, factoryPath); got != beforeFactory {
		t.Fatalf("factory.yaml was mutated:\nbefore:\n%s\nafter:\n%s", beforeFactory, got)
	}
	if got := readFactoryConvertCLIFile(t, pipelinePath); got != beforePipeline {
		t.Fatalf("pipeline.yaml was mutated:\nbefore:\n%s\nafter:\n%s", beforePipeline, got)
	}
	if got := readFactoryConvertCLIFile(t, extraPath); got != beforeExtra {
		t.Fatalf("template file was mutated:\nbefore:\n%s\nafter:\n%s", beforeExtra, got)
	}
}

func TestFactoryConvertCLIAllowsEquivalentLegacyTemplateFile(t *testing.T) {
	withTempWorkingDir(t)
	writeFactoryConvertCLIWorkspaceConfig(t, "engineering", "workspace_secret")
	writeFactoryConvertCLIFactory(t, "linear-triage", `
name: linear-triage
integration: linear
template: engineering
workspace: product
trigger_status: Ready for Agent
`, validFactoryConvertCLIPipeline("move_issue: In Progress"))
	writeFactoryConvertCLIFile(t, filepath.Join(".elasticclaw", "factories", "linear-triage", "BOOTSTRAP.md"), "legacy bootstrap\n")
	writeFactoryConvertCLIFile(t, filepath.Join(".elasticclaw", "workspaces", "engineering", "BOOTSTRAP.md"), "legacy bootstrap\n")

	out, err := executeFactoryCommand(t, "convert", "linear-triage", "--workspace", "engineering")
	if err != nil {
		t.Fatalf("factory convert: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "linear-triage.yaml")); err != nil {
		t.Fatalf("workflow was not written: %v", err)
	}
}

func executeFactoryCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := FactoryCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func writeFactoryConvertCLIWorkspaceConfig(t *testing.T, name string, secrets ...string) {
	t.Helper()

	dir := filepath.Join(".elasticclaw", "workspaces", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	var b strings.Builder
	b.WriteString("schema_version: v1\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("provider: replicated\n")
	if len(secrets) > 0 {
		b.WriteString("secrets:\n")
		for _, secret := range secrets {
			b.WriteString("  - " + secret + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(b.String()), 0644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}
}

func writeFactoryConvertCLIFactory(t *testing.T, name, factoryYAML, pipelineYAML string) (string, string) {
	t.Helper()

	dir := filepath.Join(".elasticclaw", "factories", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir factory: %v", err)
	}
	factoryPath := filepath.Join(dir, "factory.yaml")
	if err := os.WriteFile(factoryPath, []byte(strings.TrimSpace(factoryYAML)+"\n"), 0644); err != nil {
		t.Fatalf("write factory.yaml: %v", err)
	}
	pipelinePath := filepath.Join(dir, "pipeline.yaml")
	if err := os.WriteFile(pipelinePath, []byte(pipelineYAML), 0644); err != nil {
		t.Fatalf("write pipeline.yaml: %v", err)
	}
	return factoryPath, pipelinePath
}

func readFactoryConvertCLIFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeFactoryConvertCLIFile(t *testing.T, path string, data string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func factoryConvertResultHasDiagnostic(result factoryConvertResult, id string) bool {
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.ID == id {
			return true
		}
	}
	return false
}

func validFactoryConvertCLIPipeline(action string) string {
	return "stages:\n" +
		"  - id: working\n" +
		"    entry: true\n" +
		"    on_enter:\n" +
		"      " + action + "\n"
}
