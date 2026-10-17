package v2

import (
	"encoding/base64"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDependencyUpdateCommandIncludesConfig(t *testing.T) {
	cfg := DependencyUpdateConfig{
		Ecosystems: []string{"go", "npm"},
		Grouping:   "all",
		Paths:      []string{"."},
	}
	cmd, err := BuildDependencyUpdateCommand(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "python3 - <<'PY'") {
		t.Fatalf("command = %s", cmd)
	}
	if strings.Contains(cmd, "__CONFIG_B64__") {
		t.Fatalf("command = %s", cmd)
	}
	// The base64 marker should have been replaced by the actual encoded config.
	encoded := extractConfigBase64(cmd)
	if encoded == "" {
		t.Fatal("no encoded config found")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var parsed DependencyUpdateConfig
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Ecosystems) != 2 || parsed.Ecosystems[0] != "go" || parsed.Ecosystems[1] != "npm" {
		t.Fatalf("ecosystems = %v", parsed.Ecosystems)
	}
	if parsed.Grouping != "all" {
		t.Fatalf("grouping = %q", parsed.Grouping)
	}
}

func TestNormalizeDependencyUpdateConfigDefaults(t *testing.T) {
	cfg := DependencyUpdateConfig{Ecosystems: []string{"  go  "}}
	out := normalizeDependencyUpdateConfig(cfg)
	if len(out.Ecosystems) != 1 || out.Ecosystems[0] != "go" {
		t.Fatalf("ecosystems = %v", out.Ecosystems)
	}
	if out.Grouping != "all" {
		t.Fatalf("grouping = %q", out.Grouping)
	}
	if len(out.Paths) != 1 || out.Paths[0] != "." {
		t.Fatalf("paths = %v", out.Paths)
	}
	if len(out.Allow) != 1 || out.Allow[0] != "*" {
		t.Fatalf("allow = %v", out.Allow)
	}
	if out.SeparateMajor == nil || !*out.SeparateMajor {
		t.Fatal("expected separate_major to default to true")
	}
	if out.SeparateSecurity == nil || !*out.SeparateSecurity {
		t.Fatal("expected separate_security to default to true")
	}
	if out.SeparateRuntime == nil || !*out.SeparateRuntime {
		t.Fatal("expected separate_runtime to default to true")
	}
	if out.IncludeIndirect {
		t.Fatal("expected include_indirect to default to false")
	}
}

func TestBuildDependencyUpdateCommandIncludesIncludeIndirect(t *testing.T) {
	cfg := DependencyUpdateConfig{
		Ecosystems:      []string{"go"},
		IncludeIndirect: true,
	}
	cmd, err := BuildDependencyUpdateCommand(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(extractConfigBase64(cmd))
	if err != nil {
		t.Fatal(err)
	}
	var parsed DependencyUpdateConfig
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.IncludeIndirect {
		t.Fatal("expected include_indirect to survive config encoding")
	}
	if !strings.Contains(cmd, "include_indirect") {
		t.Fatal("expected embedded script to gate on include_indirect")
	}
}

// TestDependencyUpdateCommandRecordsIndirectSkips runs the embedded update
// script against a fake go toolchain. With include_indirect unset, indirect
// modules must be recorded as skipped (like every other skip path) rather
// than silently dropped from the receipt; with it set, indirect patch
// updates apply while indirect minor/major stay skipped.
func TestDependencyUpdateCommandRecordsIndirectSkips(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, bin, "go", `#!/usr/bin/env bash
if [ "$*" = "list -e -m -u -json all" ]; then
  printf '%s\n' '{"Path":"example.com/root","Version":"v1.0.0","Update":{"Version":"v1.0.1"}}'
  printf '%s\n' '{"Path":"example.com/indirectpatch","Version":"v1.0.0","Update":{"Version":"v1.0.1"},"Indirect":true}'
  printf '%s\n' '{"Path":"example.com/indirectminor","Version":"v1.0.0","Update":{"Version":"v1.1.0"},"Indirect":true}'
  exit 0
fi
if [ "$1" = "get" ]; then
  case "$2" in
    example.com/root@v1.0.1|example.com/indirectpatch@v1.0.1)
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
	writeTestFile(t, root, "go.mod", "module example.com/root\n")
	writeTestFile(t, root, "go.sum", "")

	run := func(t *testing.T, cfg DependencyUpdateConfig) map[string]interface{} {
		t.Helper()
		command, err := BuildDependencyUpdateCommand(cfg)
		if err != nil {
			t.Fatalf("build command: %v", err)
		}
		cmd := osexec.Command("bash", "-c", command)
		cmd.Dir = root
		cmd.Env = testPathEnv(bin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dependency update command failed: %v\n%s", err, out)
		}
		return parseLastJSONLine(t, string(out))
	}

	parsed := run(t, DependencyUpdateConfig{Ecosystems: []string{"go"}})
	assertDepUpdate(t, parsed, "example.com/root", true, "")
	assertDepUpdate(t, parsed, "example.com/indirectpatch", false, "indirect updates disabled")
	assertDepUpdate(t, parsed, "example.com/indirectminor", false, "indirect updates disabled")

	parsed = run(t, DependencyUpdateConfig{Ecosystems: []string{"go"}, IncludeIndirect: true})
	assertDepUpdate(t, parsed, "example.com/root", true, "")
	assertDepUpdate(t, parsed, "example.com/indirectpatch", true, "")
	assertDepUpdate(t, parsed, "example.com/indirectminor", false, "indirect updates are limited to patch releases")
}

func writeTestExecutable(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testPathEnv(bin string) []string {
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// parseLastJSONLine extracts the result document the update script prints as
// its final stdout line; CombinedOutput interleaves the script's stderr
// progress lines, so scan from the end for the JSON object.
func parseLastJSONLine(t *testing.T, output string) map[string]interface{} {
	t.Helper()
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err == nil {
			if _, ok := parsed["updates"]; ok {
				return parsed
			}
		}
	}
	t.Fatalf("no result JSON in output:\n%s", output)
	return nil
}

func assertDepUpdate(t *testing.T, parsed map[string]interface{}, name string, applied bool, skippedReason string) {
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
		if update["name"] != name {
			continue
		}
		if update["applied"] != applied {
			t.Fatalf("%s applied = %#v, want %v", name, update["applied"], applied)
		}
		if skippedReason != "" && update["skipped_reason"] != skippedReason {
			t.Fatalf("%s skipped_reason = %#v, want %q", name, update["skipped_reason"], skippedReason)
		}
		return
	}
	b, _ := json.MarshalIndent(parsed["updates"], "", "  ")
	t.Fatalf("missing update %s in:\n%s", name, b)
}

func extractConfigBase64(command string) string {
	// The command embeds the base64 config between the marker __CONFIG_B64__ in the Python script.
	// After replacement, the script still contains CONFIG = json.loads(base64.b64decode("...").decode("utf-8"))
	// so we can extract the quoted string.
	start := strings.Index(command, "b64decode(\"")
	if start < 0 {
		return ""
	}
	start += len("b64decode(\"")
	end := strings.Index(command[start:], "\")")
	if end < 0 {
		return ""
	}
	return command[start : start+end]
}
