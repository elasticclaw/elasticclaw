package daytona

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	daytonaerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	providertypes "github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestBuildOpenClawEnvFileIncludesWorkflowSecrets(t *testing.T) {
	env, err := buildOpenClawEnvFile(map[string]string{
		"ELASTICCLAW_HUB_URL":   "https://hub.example.com",
		"AWS_SECRET_ACCESS_KEY": "secret-with-'quote",
		"AWS_ACCESS_KEY_ID":     "workflow-access-key",
	})
	if err != nil {
		t.Fatalf("buildOpenClawEnvFile: %v", err)
	}

	content := string(env)
	for _, want := range []string{
		"export AWS_ACCESS_KEY_ID='workflow-access-key'",
		"export AWS_SECRET_ACCESS_KEY='secret-with-'\"'\"'quote'",
		"export ELASTICCLAW_HUB_URL='https://hub.example.com'",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("OpenClaw env file missing expected variable")
		}
	}
	if strings.Index(content, "AWS_ACCESS_KEY_ID") > strings.Index(content, "AWS_SECRET_ACCESS_KEY") {
		t.Fatal("environment file should be deterministic")
	}
}

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

func TestIsTransientExecError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "request timeout", err: fmt.Errorf("execute: %w", daytonaerrors.NewDaytonaError("request timeout", http.StatusRequestTimeout, nil)), want: true},
		{name: "rate limited", err: daytonaerrors.NewDaytonaError("slow down", http.StatusTooManyRequests, nil), want: true},
		{name: "server error", err: daytonaerrors.NewDaytonaError("unavailable", http.StatusServiceUnavailable, nil), want: true},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "not found", err: daytonaerrors.NewDaytonaError("missing", http.StatusNotFound, nil), want: false},
		{name: "ordinary error", err: fmt.Errorf("permission denied"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransientExecError(tt.err); got != tt.want {
				t.Fatalf("IsTransientExecError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestInstanceStatus(t *testing.T) {
	tests := []struct {
		name  string
		state daytona.SandboxState
		want  providertypes.InstanceStatus
	}{
		{name: "started", state: daytona.SandboxStateStarted, want: providertypes.StatusRunning},
		{name: "stopped", state: daytona.SandboxStateStopped, want: providertypes.StatusStopped},
		{name: "paused", state: daytona.SandboxStatePaused, want: providertypes.StatusStopped},
		{name: "archived", state: daytona.SandboxStateArchived, want: providertypes.StatusStopped},
		{name: "error", state: daytona.SandboxStateError, want: providertypes.StatusError},
		{name: "building", state: daytona.SandboxStatePendingBuild, want: providertypes.StatusStarting},
		{name: "build failed", state: daytona.SandboxStateBuildFailed, want: providertypes.StatusError},
		{name: "unknown", state: daytona.SandboxStateUnknown, want: providertypes.StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := instanceStatus(tt.state); got != tt.want {
				t.Fatalf("instanceStatus(%q) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestIsDaytonaNotFound(t *testing.T) {
	if !isDaytonaNotFound(fmt.Errorf("get sandbox: %w", daytonaerrors.NewDaytonaNotFoundError("missing", nil))) {
		t.Fatal("wrapped DaytonaNotFoundError should be recognized")
	}
	if isDaytonaNotFound(daytonaerrors.NewDaytonaError("not found in unrelated message", http.StatusBadRequest, nil)) {
		t.Fatal("non-404 Daytona error should not be recognized as not found")
	}
}

func TestTruncateFileCommandQuotesPath(t *testing.T) {
	// Empty content cannot go through the upload API, which rejects it with
	// "bad request: http: no such file" — a 0-byte .gitkeep or SQLite -wal in a
	// checkpoint used to fail the entire restore because of it.
	got := truncateFileCommand("/home/daytona/.openclaw/workspace/$SECRET/`touch /tmp/pwn`/it's.txt")
	want := `: > '/home/daytona/.openclaw/workspace/$SECRET/` + "`touch /tmp/pwn`" + `/it'"'"'s.txt'`
	if got != want {
		t.Fatalf("truncateFileCommand = %q, want %q", got, want)
	}
	// A "bash -c" prefix here would nest a second shell inside the one
	// ExecWithTimeout adds, and the path would never reach tail/truncate.
	if strings.HasPrefix(got, "bash ") || strings.Contains(got, `> "`) {
		t.Fatalf("expected a bare single-quoted redirect, got %q", got)
	}
}
