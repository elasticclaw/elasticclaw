package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types/convert"
	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestRunConvertWorkspaceInPlace(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "my-ws")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	v1 := []byte(`schema_version: v1
name: my-ws
provider: daytona
repositories:
  - repo: org/repo
    permissions: write
`)
	cfgPath := filepath.Join(wsDir, "elasticclaw-config.yaml")
	if err := os.WriteFile(cfgPath, v1, 0644); err != nil {
		t.Fatal(err)
	}

	f := newConvertFlags()
	f.to = "2"
	f.inPlace = true
	if err := runConvert(convert.KindWorkspace, wsDir, f); err != nil {
		t.Fatalf("runConvert: %v", err)
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "schema_version: 2") {
		t.Fatalf("expected v2 output, got:\n%s", out)
	}
	if _, err := v2.ParseAndValidateWorkspace(out); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestRunConvertWorkflowToOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "wf.yaml")
	out := filepath.Join(dir, "wf.v2.yaml")
	v1 := []byte(`schema_version: v1
name: demo
stages:
  - id: start
    entry: true
  - id: done
    terminal: true
    triggers:
      - pr_merged: {}
`)
	if err := os.WriteFile(in, v1, 0644); err != nil {
		t.Fatal(err)
	}
	f := newConvertFlags()
	f.to = "2"
	f.output = out
	if err := runConvert(convert.KindWorkflow, in, f); err != nil {
		t.Fatalf("runConvert: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v2.ParseAndValidateWorkflow(data); err != nil {
		t.Fatalf("validate: %v\n%s", err, data)
	}
}

func TestReadConvertInputWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "elasticclaw-config.yaml")
	if err := os.WriteFile(path, []byte("schema_version: v1\nname: x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gotPath, data, err := readConvertInput(convert.KindWorkspace, dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	if !strings.Contains(string(data), "name: x") {
		t.Fatalf("data = %s", data)
	}
}
