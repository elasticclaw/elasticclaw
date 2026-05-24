package daytona

import (
	"strings"
	"testing"
)

func TestBuildStartOpenClawCommandQuotesWorkdir(t *testing.T) {
	workdir := `/tmp/a'; touch /tmp/pwned; #'`

	cmd := buildStartOpenClawCommand(workdir)

	if !strings.HasPrefix(cmd, "bash -c '") {
		t.Fatalf("command should pass a quoted script to bash -c: %s", cmd)
	}
	if strings.Contains(cmd, "cd /tmp/a'; touch /tmp/pwned; #'") {
		t.Fatalf("workdir was interpolated without shell quoting: %s", cmd)
	}
	if !strings.Contains(cmd, `cd '"'"'/tmp/a'"'"'"'"'"'"'"'"'; touch /tmp/pwned; #'"'"'"'"'"'"'"'"''"'"'`) {
		t.Fatalf("workdir was not preserved as one quoted cd target: %s", cmd)
	}
	if !strings.Contains(cmd, "&& { source ~/.openclaw/env 2>/dev/null || true; setsid nohup") {
		t.Fatalf("gateway start is not guarded by successful cd: %s", cmd)
	}
}

func TestShellEnvNameValidation(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "_TOKEN", "A1"} {
		if !shellEnvNameRE.MatchString(name) {
			t.Fatalf("valid env name %q rejected", name)
		}
	}
	for _, name := range []string{"1BAD", "BAD NAME", "BAD;touch", "BAD$(cmd)"} {
		if shellEnvNameRE.MatchString(name) {
			t.Fatalf("invalid env name %q accepted", name)
		}
	}
}
