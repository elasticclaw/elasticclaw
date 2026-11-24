//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	daytonaProvider "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
)

const daytonaDiagnosticTimeout = 20 * time.Second

// daytonaDiagnostics are read from a failed run's sandbox before cleanup
// destroys it. Every command tolerates missing files and permissions.
var daytonaDiagnostics = []struct {
	name    string
	command string
}{
	{"gateway.log", daytonaProvider.GatewayLogTailCommand},
	{"claw-bridge.log", daytonaProvider.BridgeLogTailCommand},
	{"openclaw-install.log", "tail -n 200 /tmp/openclaw-install.log 2>&1"},
	{"memory.txt", "free -m 2>&1; for f in memory.max memory.peak memory.events; do echo \"== /sys/fs/cgroup/$f\"; cat /sys/fs/cgroup/$f 2>&1; done"},
	{"dmesg.txt", "{ sudo -n dmesg 2>/dev/null || dmesg 2>&1; } | tail -n 100"},
	{"ps.txt", "ps aux --sort=-rss 2>&1 | head -n 25"},
}

// collectDaytonaDiagnosticsOnFailure saves gateway/bridge logs and memory
// state from the claw's Daytona sandbox when the test failed. It is
// best-effort and time-boxed: it logs problems and never fails the test.
//
// Register it with t.Cleanup after every other cleanup so it runs first,
// before the provider sweep and agent deletion destroy the sandbox.
func collectDaytonaDiagnosticsOnFailure(t *testing.T, env e2eEnv, hub *hubProcess, agentID string) {
	t.Helper()
	if !t.Failed() || env.SandboxProvider != "daytona" || agentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	provider, sandboxID := hub.agentProvider(ctx, t, agentID)
	if provider != "daytona" || sandboxID == "" {
		t.Logf("daytona diagnostics: no sandbox recorded for agent %s", agentID)
		return
	}
	p, err := daytonaProvider.New(map[string]interface{}{"api_key": env.DaytonaAPIKey})
	if err != nil {
		t.Logf("daytona diagnostics: create provider: %v", err)
		return
	}

	dir := strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_DIAGNOSTICS_DIR"))
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Logf("daytona diagnostics: create %s: %v", dir, err)
			dir = ""
		}
	}
	for _, d := range daytonaDiagnostics {
		if ctx.Err() != nil {
			t.Logf("daytona diagnostics: stopped early: %v", ctx.Err())
			return
		}
		output := runDaytonaDiagnostic(ctx, p, sandboxID, d.command)
		if dir == "" {
			t.Logf("=== daytona sandbox %s %s ===\n%s", sandboxID, d.name, output)
			continue
		}
		path := filepath.Join(dir, "daytona-"+sandboxID+"-"+d.name)
		if err := os.WriteFile(path, []byte(output), 0o644); err != nil {
			t.Logf("daytona diagnostics: write %s: %v", path, err)
			continue
		}
		t.Logf("daytona diagnostics: saved %s (%d bytes)", path, len(output))
	}
}

func runDaytonaDiagnostic(ctx context.Context, p *daytonaProvider.Provider, sandboxID, command string) string {
	execCtx, cancel := context.WithTimeout(ctx, daytonaDiagnosticTimeout)
	defer cancel()
	result, err := p.ExecWithTimeout(execCtx, sandboxID, []string{command}, daytonaDiagnosticTimeout)
	if err != nil {
		return fmt.Sprintf("<exec failed: %v>\n", err)
	}
	if result.ExitCode != 0 {
		return fmt.Sprintf("%s\n<exit %d>\n", result.Stdout, result.ExitCode)
	}
	return result.Stdout
}
