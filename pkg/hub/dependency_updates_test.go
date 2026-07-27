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

func TestDependencyUpdatesCommandReturnsPythonScriptDirectly(t *testing.T) {
	command, err := buildDependencyUpdatesCommand(pipeline.DependencyUpdatesAction{Enabled: true})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	if strings.HasPrefix(command, "nix develop --accept-flake-config -c bash -lc ") {
		t.Fatalf("dependency update command should not add its own nix develop wrapper; the workspace run wrapper handles nix:\n%s", command)
	}
	if !strings.HasPrefix(command, "python3 - <<") {
		t.Fatalf("command missing dependency update script:\n%s", command)
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
  if [ "$2" != "example.com/root@v1.1.0" ]; then
    echo "unexpected go get target: $2" >&2
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

func TestDependencyUpdatesGoGetOneAtATime(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
ROOT=$(dirname "$0")/..
if [ "$*" = "list -m -u -json all" ]; then
  printf '%s\n' '{"Path":"example.com/root","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  printf '%s\n' '{"Path":"example.com/other","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  exit 0
fi
if [ "$1" = "get" ]; then
  case "$2" in
    example.com/root@v1.1.0|example.com/other@v1.1.0)
      echo changed >> go.sum
      exit 0
      ;;
    *)
      echo "unexpected go get target: $2" >&2
      exit 1
      ;;
  esac
fi
if [ "$1 $2" = "mod tidy" ]; then
  exit 0
fi
exit 0
`)
	writeExecutable(t, bin, "npm", `#!/usr/bin/env bash
if [ "$1 $2" = "outdated --json" ]; then
  printf '%s\n' '{}'
  exit 1
fi
if [ "$1 $2" = "update --package-lock-only" ]; then
  exit 0
fi
exit 0
`)
	writeFile(t, root, "go.mod", "module example.com/root\n")
	writeFile(t, root, "go.sum", "")

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
	commands, ok := parsed["commands"].([]interface{})
	if !ok {
		t.Fatalf("commands = %#v, want list", parsed["commands"])
	}
	var getAttempts int
	for _, raw := range commands {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		cmdStr, _ := c["command"].(string)
		if strings.HasPrefix(cmdStr, "go get") {
			getAttempts++
		}
	}
	if getAttempts != 2 {
		t.Fatalf("go get commands = %d, want 2 one-at-a-time commands", getAttempts)
	}
	assertUpdateStatus(t, parsed, "go", "example.com/root", true, "")
	assertUpdateStatus(t, parsed, "go", "example.com/other", true, "")
}

func TestDependencyUpdatesSkipsReplacedIndirectAndInvalidVersions(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
if [ "$*" = "list -m -u -json all" ]; then
  printf '%s\n' '{"Path":"example.com/root","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  printf '%s\n' '{"Path":"example.com/replaced","Version":"v0.0.0","Update":{"Version":"v1.0.0"},"Replace":{"Path":"example.com/replaced","Version":"v0.0.0"}}'
  printf '%s\n' '{"Path":"example.com/indirect","Version":"v1.0.0","Update":{"Version":"v1.1.0"},"Indirect":true}'
  printf '%s\n' '{"Path":"example.com/invalid","Version":"v0.0.0","Update":{"Version":"v0.0.0"}}'
  exit 0
fi
if [ "$1" = "get" ]; then
  if [ "$2" != "example.com/root@v1.1.0" ]; then
    echo "unexpected go get target: $2" >&2
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
if [ "$1 $2" = "outdated --json" ]; then
  printf '%s\n' '{}'
  exit 1
fi
if [ "$1 $2" = "update --package-lock-only" ]; then
  exit 0
fi
exit 0
`)
	writeFile(t, root, "go.mod", "module example.com/root\n")
	writeFile(t, root, "go.sum", "")

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
	assertUpdateStatus(t, parsed, "go", "example.com/root", true, "")
	assertUpdateStatus(t, parsed, "go", "example.com/invalid", false, "invalid/placeholder version")
	// Replaced and indirect modules are silently skipped and should not appear
	// in the update records at all, to avoid noisy reports for unpinned deps.
}

func TestDependencyUpdatesRetriesGoGetFailure(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, bin, "go", `#!/usr/bin/env bash
set -e
ROOT=$(dirname "$0")/..
COUNT_FILE="$ROOT/get-count"
count=0
if [ -f "$COUNT_FILE" ]; then
  count=$(cat "$COUNT_FILE")
fi
if [ "$*" = "list -m -u -json all" ]; then
  printf '%s\n' '{"Path":"example.com/root","Version":"v1.0.0","Update":{"Version":"v1.1.0"}}'
  exit 0
fi
if [ "$1" = "get" ]; then
  count=$((count + 1))
  echo "$count" > "$COUNT_FILE"
  if [ "$count" -eq 1 ]; then
    echo "go: updating go.mod: existing contents have changed since last read" >&2
    exit 1
  fi
  if [ "$2" != "example.com/root@v1.1.0" ]; then
    echo "unexpected go get target: $2" >&2
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
if [ "$1 $2" = "outdated --json" ]; then
  printf '%s\n' '{}'
  exit 1
fi
if [ "$1 $2" = "update --package-lock-only" ]; then
  exit 0
fi
exit 0
`)
	writeFile(t, root, "go.mod", "module example.com/root\n")
	writeFile(t, root, "go.sum", "")

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
	commands, ok := parsed["commands"].([]interface{})
	if !ok {
		t.Fatalf("commands = %#v, want list", parsed["commands"])
	}
	var getAttempts int
	for _, raw := range commands {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		cmdStr, _ := c["command"].(string)
		if strings.HasPrefix(cmdStr, "go get") {
			getAttempts++
		}
	}
	if getAttempts != 2 {
		t.Fatalf("go get attempts = %d, want 2 (initial failure + retry)", getAttempts)
	}
	assertUpdateStatus(t, parsed, "go", "example.com/root", true, "")
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
  if [ "$2" = "ignored.com/risky@v1.1.0" ] || [ "$2" = "example.com/major@v1.0.0" ]; then
    echo "go get included skipped dependency: $2" >&2
    exit 1
  fi
  if [ "$2" != "example.com/root@v1.1.0" ]; then
    echo "go get omitted or unexpected selected dependency: $2" >&2
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

func TestFormatDependencyUpdateFailureExtractsFailedCommand(t *testing.T) {
	stdout := `{"commands":[{"command":"go list -m -u -json all","cwd":"ec/e2e","exit_code":0,"stderr":""},{"command":"go get example.com/foo@v1.1.0","cwd":"ec/e2e","exit_code":1,"stderr":"go: example.com/foo@v1.1.0: not found"}],"updates":[]}`
	result := &pipelineRunResult{ExitCode: 1, Stdout: stdout}
	got := formatDependencyUpdateFailure(result)
	want := "Dependency update step failed: go get example.com/foo@v1.1.0 (cwd=ec/e2e): go: example.com/foo@v1.1.0: not found"
	if got != want {
		t.Fatalf("formatDependencyUpdateFailure = %q, want %q", got, want)
	}
}

func TestFormatDependencyUpdateFailureFallsBackToRawOutput(t *testing.T) {
	result := &pipelineRunResult{ExitCode: 1, Stderr: "some shellHook banner\n" + strings.Repeat("x", 3000)}
	got := formatDependencyUpdateFailure(result)
	if !strings.HasPrefix(got, "Dependency update step failed: ") {
		t.Fatalf("unexpected prefix: %q", got)
	}
	if len(got) > 2100 {
		t.Fatalf("fallback message not truncated: length %d", len(got))
	}
}

func TestFormatDependencyUpdateFailureHandlesMultiLineJSON(t *testing.T) {
	stdout := "some shellHook banner\n" + `{
  "commands": [
    {
      "command": "go get example.com/foo@v1.1.0",
      "cwd": "ec/e2e",
      "exit_code": 1,
      "stderr": "go: example.com/foo@v1.1.0: not found"
    }
  ]
}`
	result := &pipelineRunResult{ExitCode: 1, Stdout: stdout}
	got := formatDependencyUpdateFailure(result)
	want := "Dependency update step failed: go get example.com/foo@v1.1.0 (cwd=ec/e2e): go: example.com/foo@v1.1.0: not found"
	if got != want {
		t.Fatalf("formatDependencyUpdateFailure = %q, want %q", got, want)
	}
}

func TestFormatDependencyUpdateFailureSkipsAllowedExitCodes(t *testing.T) {
	stdout := `{"commands":[{"command":"npm outdated --json","cwd":"ec/e2e","exit_code":1,"failed":false},{"command":"npm update --package-lock-only foo","cwd":"ec/e2e","exit_code":1,"stderr":"npm ERR! code E404","failed":true}],"updates":[]}`
	result := &pipelineRunResult{ExitCode: 1, Stdout: stdout}
	got := formatDependencyUpdateFailure(result)
	want := "Dependency update step failed: npm update --package-lock-only foo (cwd=ec/e2e): npm ERR! code E404"
	if got != want {
		t.Fatalf("formatDependencyUpdateFailure = %q, want %q", got, want)
	}
}

func TestFormatDependencyUpdateFailureSkipsRetriedAttempts(t *testing.T) {
	stdout := `{"commands":[{"command":"go get example.com/foo@v1.1.0","cwd":"ec/e2e","exit_code":1,"stderr":"existing contents have changed since last read","failed":false},{"command":"go get example.com/foo@v1.1.0","cwd":"ec/e2e","exit_code":0,"failed":false},{"command":"go mod tidy","cwd":"ec/e2e","exit_code":1,"stderr":"go: errors","failed":true}],"updates":[]}`
	result := &pipelineRunResult{ExitCode: 1, Stdout: stdout}
	got := formatDependencyUpdateFailure(result)
	want := "Dependency update step failed: go mod tidy (cwd=ec/e2e): go: errors"
	if got != want {
		t.Fatalf("formatDependencyUpdateFailure = %q, want %q", got, want)
	}
}
