package hub

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
)

func TestNormalizeDependencyUpdatesConfigDefaults(t *testing.T) {
	cfg := normalizeDependencyUpdatesConfig(pipeline.DependencyUpdatesAction{Enabled: true})

	if strings.Join(cfg.Ecosystems, ",") != "auto" {
		t.Fatalf("ecosystems = %#v, want auto", cfg.Ecosystems)
	}
	if strings.Join(cfg.Paths, ",") != "." {
		t.Fatalf("paths = %#v, want .", cfg.Paths)
	}
	if cfg.Grouping != "all" {
		t.Fatalf("grouping = %q, want all", cfg.Grouping)
	}
	if cfg.IncludeMajor {
		t.Fatal("include_major default should be false")
	}
	if !cfg.SeparateMajor || !cfg.SeparateSecurity || !cfg.SeparateRuntime {
		t.Fatalf("separate defaults not enabled: %#v", cfg)
	}
	if strings.Join(cfg.Allow, ",") != "*" {
		t.Fatalf("allow = %#v, want *", cfg.Allow)
	}
	if dependencyUpdatesOutputName(pipeline.DependencyUpdatesAction{}) != defaultDependencyUpdatesOutput {
		t.Fatalf("default output mismatch")
	}
}

func TestDiscoverDependencyUpdateManifests(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/root\n")
	writeFile(t, root, "go.sum", "")
	writeFile(t, root, "web/package.json", `{"name":"web"}`)
	writeFile(t, root, "web/package-lock.json", `{"lockfileVersion":3}`)
	writeFile(t, root, "web/node_modules/ignored/package.json", `{"name":"ignored"}`)

	manifests, err := discoverDependencyUpdateManifests(root, normalizeDependencyUpdatesConfig(pipeline.DependencyUpdatesAction{Enabled: true}))
	if err != nil {
		t.Fatalf("discover manifests: %v", err)
	}
	got := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		got = append(got, manifest.Ecosystem+":"+manifest.Path+":"+strings.Join(manifest.Lockfiles, ","))
	}
	want := []string{
		"go:go.mod:go.sum",
		"npm:web/package.json:web/package-lock.json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("manifests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDiscoverDependencyUpdateManifestsRejectsEscapingPath(t *testing.T) {
	root := t.TempDir()
	_, err := discoverDependencyUpdateManifests(root, dependencyUpdatesConfig{
		Ecosystems: []string{"go"},
		Paths:      []string{"../outside"},
	})
	if err == nil {
		t.Fatal("expected escaping path error")
	}
}

func TestDependencyUpdatesGeneratedCommandOutputsStructuredJSON(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
if [ "$*" = "list -m -u -json all" ]; then
  printf '%s\n' '{"Path":"example.com/root","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  exit 0
fi
if [ "$1 $2" = "get -u" ]; then
  echo changed >> go.sum
  exit 0
fi
if [ "$1 $2" = "mod tidy" ]; then
  exit 0
fi
exit 0
`)
	writeExecutable(t, bin, "npm", `#!/usr/bin/env bash
set -e
if [ "$1 $2" = "outdated --json" ]; then
  printf '%s\n' '{"left-pad":{"current":"1.0.0","wanted":"1.1.0","latest":"2.0.0"}}'
  exit 1
fi
if [ "$1 $2" = "update --package-lock-only" ]; then
  printf '%s\n' '{"lockfileVersion":3,"updated":true}' > package-lock.json
  exit 0
fi
exit 0
`)
	writeFile(t, root, "go.mod", "module example.com/root\n")
	writeFile(t, root, "go.sum", "")
	writeFile(t, root, "package.json", `{"name":"root","dependencies":{"left-pad":"^1.0.0"}}`)
	writeFile(t, root, "package-lock.json", `{"lockfileVersion":3}`)

	command, err := buildDependencyUpdatesCommand(pipeline.DependencyUpdatesAction{Enabled: true})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	cmd := osexec.Command("bash", "-lc", command)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dependency update command failed: %v\n%s", err, out)
	}

	parsed, ok := parsePipelineOutputJSON(string(out))
	if !ok {
		t.Fatalf("command did not emit JSON:\n%s", out)
	}
	if parsed["ecosystems"] == nil || parsed["manifests"] == nil || parsed["updates"] == nil || parsed["commands"] == nil || parsed["files_changed"] == nil {
		b, _ := json.MarshalIndent(parsed, "", "  ")
		t.Fatalf("output missing structured fields:\n%s", b)
	}
	filesChanged, ok := parsed["files_changed"].([]interface{})
	if !ok || len(filesChanged) == 0 {
		t.Fatalf("files_changed = %#v, want at least one changed file", parsed["files_changed"])
	}
	updates, ok := parsed["updates"].([]interface{})
	if !ok || len(updates) < 2 {
		t.Fatalf("updates = %#v, want applied and skipped update records", parsed["updates"])
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
