package v2

import (
	"encoding/base64"
	"encoding/json"
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
