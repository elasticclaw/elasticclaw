package hub

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
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

func TestWrapDependencyUpdatesCommandUsesNixDevShellWhenEnabled(t *testing.T) {
	command := "python3 - <<'PY'\nprint('dependency update')\nPY"

	wrapped := wrapDependencyUpdatesCommand(command, true)

	want := "nix develop --accept-flake-config -c bash -lc " + shellQuote(command)
	if wrapped != want {
		t.Fatalf("wrapped command = %q, want %q", wrapped, want)
	}
	if got := wrapDependencyUpdatesCommand(command, false); got != command {
		t.Fatalf("non-nix command = %q, want original command", got)
	}
}

func TestDependencyUpdatesCommandForClawWrapsNixEnabledAgents(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, nix, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		"nix-claw", "test-tenant-id", "nix claw", "workspace", "connected", 1,
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	command, err := s.dependencyUpdatesCommandForClaw("nix-claw", pipeline.DependencyUpdatesAction{Enabled: true})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	if !strings.HasPrefix(command, "nix develop --accept-flake-config -c bash -lc ") {
		t.Fatalf("command was not wrapped with nix develop:\n%s", command)
	}
	if !strings.Contains(command, "python3 - <<") {
		t.Fatalf("wrapped command missing dependency update script:\n%s", command)
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
if [ "$1" = "get" ]; then
  if [[ "$*" != *"example.com/root@v1.1.0"* ]]; then
    echo "missing selected module in go get: $*" >&2
    exit 1
  fi
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
	cmd := osexec.Command("bash", "-c", command)
	cmd.Dir = root
	cmd.Env = testEnvWithPath(bin)
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

func TestDependencyUpdatesGeneratedCommandHonorsFiltersAndMajorPolicy(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
if [ "$*" = "list -m -u -json all" ]; then
  printf '%s\n' '{"Path":"example.com/root","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  printf '%s\n' '{"Path":"ignored.com/risky","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  printf '%s\n' '{"Path":"example.com/major","Version":"v0.9.0","Update":{"Version":"v1.0.0"}}'
  exit 0
fi
if [ "$1" = "get" ]; then
  if [[ "$*" == *"ignored.com/risky"* || "$*" == *"example.com/major"* ]]; then
    echo "go get included skipped dependency: $*" >&2
    exit 1
  fi
  if [[ "$*" != *"example.com/root@v1.1.0"* ]]; then
    echo "go get omitted selected dependency: $*" >&2
    exit 1
  fi
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
  printf '%s\n' '{"left-pad":{"current":"1.0.0","wanted":"1.1.0","latest":"2.0.0"},"risky-lib":{"current":"1.0.0","wanted":"1.1.0","latest":"1.1.0"},"major-lib":{"current":"0.9.0","wanted":"1.0.0","latest":"1.0.0"}}'
  exit 1
fi
if [ "$1 $2" = "update --package-lock-only" ]; then
  if [[ "$*" == *"risky-lib"* || "$*" == *"major-lib"* ]]; then
    echo "npm update included skipped dependency: $*" >&2
    exit 1
  fi
  if [[ "$*" != *"left-pad"* ]]; then
    echo "npm update omitted selected dependency: $*" >&2
    exit 1
  fi
  printf '%s\n' '{"lockfileVersion":3,"updated":true}' > package-lock.json
  exit 0
fi
exit 0
`)
	writeFile(t, root, "go.mod", "module example.com/root\n")
	writeFile(t, root, "go.sum", "")
	writeFile(t, root, "package.json", `{"name":"root","dependencies":{"left-pad":"^1.0.0","risky-lib":"^1.0.0","major-lib":"^0.9.0"}}`)
	writeFile(t, root, "package-lock.json", `{"lockfileVersion":3}`)

	command, err := buildDependencyUpdatesCommand(pipeline.DependencyUpdatesAction{
		Enabled: true,
		Ignore:  []string{"ignored.com/risky", "risky-lib"},
	})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	cmd := osexec.Command("bash", "-c", command)
	cmd.Dir = root
	cmd.Env = testEnvWithPath(bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dependency update command failed: %v\n%s", err, out)
	}

	parsed, ok := parsePipelineOutputJSON(string(out))
	if !ok {
		t.Fatalf("command did not emit JSON:\n%s", out)
	}
	assertUpdateStatus(t, parsed, "go", "ignored.com/risky", false, "filtered by allow/ignore")
	assertUpdateStatus(t, parsed, "go", "example.com/major", false, "major updates disabled")
	assertUpdateStatus(t, parsed, "npm", "risky-lib", false, "filtered by allow/ignore")
	assertUpdateStatus(t, parsed, "npm", "major-lib", false, "major updates disabled")
}

func assertUpdateStatus(t *testing.T, parsed map[string]interface{}, ecosystem, name string, applied bool, skippedReason string) {
	t.Helper()
	updates, ok := parsed["updates"].([]interface{})
	if !ok {
		t.Fatalf("updates = %#v, want list", parsed["updates"])
	}
	for _, raw := range updates {
		update, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if update["ecosystem"] == ecosystem && update["name"] == name {
			if update["applied"] != applied {
				t.Fatalf("%s %s applied = %#v, want %v", ecosystem, name, update["applied"], applied)
			}
			if skippedReason != "" && update["skipped_reason"] != skippedReason {
				t.Fatalf("%s %s skipped_reason = %#v, want %q", ecosystem, name, update["skipped_reason"], skippedReason)
			}
			return
		}
	}
	b, _ := json.MarshalIndent(parsed["updates"], "", "  ")
	commands, _ := json.MarshalIndent(parsed["commands"], "", "  ")
	t.Fatalf("missing update %s %s in:\n%s\ncommands:\n%s", ecosystem, name, b, commands)
}

func testEnvWithPath(bin string) []string {
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
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

func TestDependencyUpdatesGeneratedCommandUsesWorkspaceRootForSubdirectories(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
if [[ "$1" == "-C" && "$2" == "web" && "$3 $4 $5 $6" == "list -m -u -json" && "$7" == "all" ]]; then
  printf '%s\n' '{"Path":"example.com/web","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  exit 0
fi
if [[ "$1" == "-C" && "$2" == "web" && "$3" == "get" && "$*" == *"example.com/web@v1.1.0"* ]]; then
  echo changed >> web/go.sum
  exit 0
fi
if [[ "$1" == "-C" && "$2" == "web" && "$3 $4" == "mod tidy" ]]; then
  exit 0
fi
echo "unexpected go invocation: $*" >&2
exit 1
`)
	writeExecutable(t, bin, "npm", `#!/usr/bin/env bash
set -e
if [[ "$1" == "--prefix" && "$2" == "web" && "$3 $4" == "outdated --json" ]]; then
  printf '%s\n' '{"left-pad":{"current":"1.0.0","wanted":"1.1.0","latest":"2.0.0"}}'
  exit 1
fi
if [[ "$1" == "--prefix" && "$2" == "web" && "$3 $4" == "update --package-lock-only" ]]; then
  printf '%s\n' '{"lockfileVersion":3,"updated":true}' > web/package-lock.json
  exit 0
fi
echo "unexpected npm invocation: $*" >&2
exit 1
`)
	writeExecutable(t, bin, "nix", `#!/usr/bin/env bash
set -e
if [[ "$1" == "develop" && "$2" == "--accept-flake-config" ]]; then
    flake_dir="$3"
    shift 3
    if [[ "$1" == "-c" && "$2" == "bash" && "$3" == "-lc" ]]; then
        command="$4"
        cd "$flake_dir" && PATH="$PATH" bash -c "$command"
        exit $?
    fi
fi
echo "unexpected nix invocation: $*" >&2
exit 1
`)
	writeFile(t, root, "flake.nix", "{ description = \"test\"; }\n")
	writeFile(t, root, "web/go.mod", "module example.com/web\n")
	writeFile(t, root, "web/go.sum", "")
	writeFile(t, root, "web/package.json", `{"name":"web","dependencies":{"left-pad":"^1.0.0"}}`)
	writeFile(t, root, "web/package-lock.json", `{"lockfileVersion":3}`)

	command, err := buildDependencyUpdatesCommand(pipeline.DependencyUpdatesAction{
		Enabled: true,
		Paths:   []string{"web"},
	})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	cmd := osexec.Command("bash", "-c", command)
	cmd.Dir = root
	cmd.Env = testEnvWithPath(bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dependency update command failed: %v\n%s", err, out)
	}

	parsed, ok := parsePipelineOutputJSON(string(out))
	if !ok {
		t.Fatalf("command did not emit JSON:\n%s", out)
	}
	assertUpdateStatus(t, parsed, "go", "example.com/web", true, "")
	assertUpdateStatus(t, parsed, "npm", "left-pad", true, "")
	filesChanged, ok := parsed["files_changed"].([]interface{})
	if !ok || len(filesChanged) == 0 {
		t.Fatalf("files_changed = %#v, want at least one changed file", parsed["files_changed"])
	}
	commands, ok := parsed["commands"].([]interface{})
	if !ok {
		t.Fatalf("commands = %#v, want list", parsed["commands"])
	}
	for _, raw := range commands {
		command, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if command["cwd"] != "web" {
			continue
		}
		cmdStr, _ := command["command"].(string)
		if strings.Contains(cmdStr, "go") && !strings.Contains(cmdStr, "-C web") {
			t.Fatalf("go command for subdirectory missing -C: %q", cmdStr)
		}
		if strings.Contains(cmdStr, "npm") && !strings.Contains(cmdStr, "--prefix web") {
			t.Fatalf("npm command for subdirectory missing --prefix: %q", cmdStr)
		}
	}
}

func TestDependencyUpdatesGeneratedCommandUsesRepoRootForSubdirectories(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
if [[ "$1" == "-C" && "$2" == "web" && "$3 $4 $5 $6" == "list -m -u -json" && "$7" == "all" ]]; then
  printf '%s\n' '{"Path":"example.com/web","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  exit 0
fi
if [[ "$1" == "-C" && "$2" == "web" && "$3" == "get" && "$*" == *"example.com/web@v1.1.0"* ]]; then
  echo changed >> web/go.sum
  exit 0
fi
if [[ "$1" == "-C" && "$2" == "web" && "$3 $4" == "mod tidy" ]]; then
  exit 0
fi
echo "unexpected go invocation: $*" >&2
exit 1
`)
	writeExecutable(t, bin, "npm", `#!/usr/bin/env bash
set -e
if [[ "$1" == "--prefix" && "$2" == "web" && "$3 $4" == "outdated --json" ]]; then
  printf '%s\n' '{"left-pad":{"current":"1.0.0","wanted":"1.1.0","latest":"2.0.0"}}'
  exit 1
fi
if [[ "$1" == "--prefix" && "$2" == "web" && "$3 $4" == "update --package-lock-only" ]]; then
  printf '%s\n' '{"lockfileVersion":3,"updated":true}' > web/package-lock.json
  exit 0
fi
echo "unexpected npm invocation: $*" >&2
exit 1
`)
	writeExecutable(t, bin, "nix", `#!/usr/bin/env bash
set -e
if [[ "$1" == "develop" && "$2" == "--accept-flake-config" ]]; then
    flake_dir="$3"
    shift 3
    if [[ "$1" == "-c" && "$2" == "bash" && "$3" == "-lc" ]]; then
        command="$4"
        cd "$flake_dir" && PATH="$PATH" bash -c "$command"
        exit $?
    fi
fi
echo "unexpected nix invocation: $*" >&2
exit 1
`)
	writeFile(t, root, "vandoor/flake.nix", "{ description = \"test\"; }\n")
	writeFile(t, root, "vandoor/web/go.mod", "module example.com/web\n")
	writeFile(t, root, "vandoor/web/go.sum", "")
	writeFile(t, root, "vandoor/web/package.json", `{"name":"web","dependencies":{"left-pad":"^1.0.0"}}`)
	writeFile(t, root, "vandoor/web/package-lock.json", `{"lockfileVersion":3}`)

	command, err := buildDependencyUpdatesCommand(pipeline.DependencyUpdatesAction{
		Enabled: true,
		Paths:   []string{"vandoor/web"},
	})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	cmd := osexec.Command("bash", "-c", command)
	cmd.Dir = root
	cmd.Env = testEnvWithPath(bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dependency update command failed: %v\n%s", err, out)
	}

	parsed, ok := parsePipelineOutputJSON(string(out))
	if !ok {
		t.Fatalf("command did not emit JSON:\n%s", out)
	}
	assertUpdateStatus(t, parsed, "go", "example.com/web", true, "")
	assertUpdateStatus(t, parsed, "npm", "left-pad", true, "")
	filesChanged, ok := parsed["files_changed"].([]interface{})
	if !ok || len(filesChanged) == 0 {
		t.Fatalf("files_changed = %#v, want at least one changed file", parsed["files_changed"])
	}
	commands, ok := parsed["commands"].([]interface{})
	if !ok {
		t.Fatalf("commands = %#v, want list", parsed["commands"])
	}
	foundRepoRoot := false
	for _, raw := range commands {
		command, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if command["cwd"] != "vandoor/web" {
			continue
		}
		cmdStr, _ := command["command"].(string)
		if strings.Contains(cmdStr, "go") && !strings.Contains(cmdStr, "-C web") {
			t.Fatalf("go command for subdirectory missing -C: %q", cmdStr)
		}
		if strings.Contains(cmdStr, "npm") && !strings.Contains(cmdStr, "--prefix web") {
			t.Fatalf("npm command for subdirectory missing --prefix: %q", cmdStr)
		}
		foundRepoRoot = true
	}
	if !foundRepoRoot {
		t.Fatalf("no commands recorded with cwd vandoor/web")
	}
}
