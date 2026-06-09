package workflowsetup

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func TestWorkflowExamples(t *testing.T) {
	expected := map[string]bool{
		"github-issue.yaml":    false,
		"linear-status.yaml":   false,
		"shortcut-status.yaml": false,
		"manual-task.yaml":     false,
	}

	examplesDir := workflowExamplesDir(t)
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("read workflow examples: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		if _, ok := expected[entry.Name()]; ok {
			expected[entry.Name()] = true
		}

		path := filepath.Join(examplesDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		var workflow types.WorkflowConfig
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&workflow); err != nil {
			t.Fatalf("%s does not parse as types.WorkflowConfig: %v", entry.Name(), err)
		}
		workflow.RawConfig = string(data)

		normalized := workflow
		if err := types.NormalizeWorkflowConfig(&normalized); err != nil {
			t.Fatalf("%s does not normalize: %v", entry.Name(), err)
		}
		if err := normalized.Validate(); err != nil {
			t.Fatalf("%s does not validate as WorkflowConfig: %v", entry.Name(), err)
		}

		resp := ValidateStatic(ValidateRequest{
			Config:          string(data),
			WorkspaceConfig: "schema_version: v1\nname: examples\n",
		})
		if resp.Summary.Critical > 0 {
			t.Fatalf("%s has critical diagnostics: %#v", entry.Name(), resp.Checks)
		}
	}

	for name, found := range expected {
		if !found {
			t.Fatalf("missing workflow example %s", name)
		}
	}
}

func workflowExamplesDir(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "examples", "workflows"))
}
