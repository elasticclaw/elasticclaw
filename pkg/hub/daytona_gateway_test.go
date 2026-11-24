package hub

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBootstrapDaytonaGatewayCommand_StartsSupervisorOnce(t *testing.T) {
	cmd := daytonaStartGatewayCommand()

	assertContains(t, cmd, "openclaw-gateway-supervisor.pid", "tracks the supervisor with a pid file")
	assertContains(t, cmd, `kill -0 "$(cat "$GATEWAY_PIDFILE")"`, "a retried bootstrap step reuses a live supervisor")
	assertContains(t, cmd, "setsid nohup bash -c ", "supervisor outlives the exec session")
	assertContains(t, cmd, "while :; do", "supervisor loop is embedded in the start command")
	assertNotContains(t, cmd, "setsid nohup openclaw gateway run", "gateway must not be started unsupervised")
	assertContains(t, cmd, "gateway ready", "start command still waits for readiness")
	assertContains(t, cmd, "cfg['gateway']['port'] = 18789", "start command still pins the gateway port")
}

func TestBootstrapDaytonaGatewayCommand_Shellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not in PATH — install it to enable this test")
	}
	for name, script := range map[string]string{
		"start":      daytonaStartGatewayCommand(),
		"supervisor": daytonaGatewaySupervisorScript(),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".sh")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("write script: %v", err)
			}
			out, err := exec.Command("shellcheck", "-s", "bash", "-e", "SC1091", "-e", "SC2016", path).CombinedOutput()
			if err != nil {
				t.Fatalf("shellcheck failed:\n%s", out)
			}
		})
	}
}

// gatewaySupervisorCmd prepares the supervisor with stub openclaw, curl and
// sleep binaries. Each gateway run appends to dir/runs; curl reports the port
// as healthy only when healthyAfterRuns > 0 and that many gateways have run.
func gatewaySupervisorCmd(t *testing.T, openclawStub string, healthyAfterRuns int) (cmd *exec.Cmd, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stubs := map[string]string{
		"openclaw": `echo run >> "$STUB_DIR/runs"` + "\n" + openclawStub,
		"sleep":    `exit 0`,
		"curl": `runs=$(wc -l < "$STUB_DIR/runs" 2>/dev/null || echo 0)
[ "$HEALTHY_AFTER" -gt 0 ] && [ "$runs" -ge "$HEALTHY_AFTER" ]`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	cmd = exec.Command("bash", "-c", daytonaGatewaySupervisorScript(), "openclaw-gateway-supervisor",
		filepath.Join(dir, "supervisor.pid"), filepath.Join(dir, "gateway.log"))
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"STUB_DIR="+dir,
		"HEALTHY_AFTER="+strconv.Itoa(healthyAfterRuns),
	)
	return cmd, dir
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("run supervisor: %v", err)
	}
	return 0
}

// runGatewaySupervisor runs the supervisor against a gateway that always
// exits 1 and reports what happened.
func runGatewaySupervisor(t *testing.T, healthyAfterRuns int) (runs int, log string, exitCode int, pidfile string) {
	t.Helper()
	cmd, dir := gatewaySupervisorCmd(t, "exit 1", healthyAfterRuns)
	exitCode = exitCodeOf(t, cmd.Run())
	data, _ := os.ReadFile(filepath.Join(dir, "runs"))
	logData, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
	return strings.Count(string(data), "run"), string(logData), exitCode, filepath.Join(dir, "supervisor.pid")
}

func TestBootstrapDaytonaGatewaySupervisor_RestartsUntilBudgetExhausted(t *testing.T) {
	runs, log, exitCode, pidfile := runGatewaySupervisor(t, 0)

	if runs != 6 {
		t.Fatalf("gateway runs = %d, want 6 (first start + 5 restarts)\nlog:\n%s", runs, log)
	}
	if exitCode != 1 {
		t.Fatalf("supervisor exit code = %d, want 1", exitCode)
	}
	assertContains(t, log, "restarting (attempt 5/5) in 80s", "backs off exponentially")
	assertContains(t, log, "restart budget exhausted after 5 attempts", "logs the exhausted budget")
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("supervisor pid file still present after exit (stat err: %v)", err)
	}
}

func TestBootstrapDaytonaGatewaySupervisor_YieldsToHealthyGateway(t *testing.T) {
	runs, log, exitCode, _ := runGatewaySupervisor(t, 1)

	if runs != 1 {
		t.Fatalf("gateway runs = %d, want 1: a gateway started elsewhere must not get a rival\nlog:\n%s", runs, log)
	}
	if exitCode != 0 {
		t.Fatalf("supervisor exit code = %d, want 0", exitCode)
	}
	assertContains(t, log, "another gateway is already serving port 18789", "logs why it yields")
}

func TestBootstrapDaytonaGatewaySupervisor_TermStopsGateway(t *testing.T) {
	cmd, dir := gatewaySupervisorCmd(t, `echo $$ > "$STUB_DIR/gateway.pid"; exec /bin/sleep 30`, 0)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	gatewayPID := ""
	for deadline := time.Now().Add(5 * time.Second); gatewayPID == "" && time.Now().Before(deadline); {
		data, _ := os.ReadFile(filepath.Join(dir, "gateway.pid"))
		gatewayPID = strings.TrimSpace(string(data))
		time.Sleep(20 * time.Millisecond)
	}
	if gatewayPID == "" {
		_ = cmd.Process.Kill()
		t.Fatal("stub gateway never started")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal supervisor: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCodeOf(t, err); code != 0 {
			t.Fatalf("supervisor exit code after TERM = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("supervisor did not exit after TERM")
	}
	if err := exec.Command("kill", "-0", gatewayPID).Run(); err == nil {
		_ = exec.Command("kill", gatewayPID).Run()
		t.Fatalf("gateway pid %s still running after supervisor TERM", gatewayPID)
	}
	if _, err := os.Stat(filepath.Join(dir, "supervisor.pid")); !os.IsNotExist(err) {
		t.Fatalf("supervisor pid file still present after TERM (stat err: %v)", err)
	}
}
