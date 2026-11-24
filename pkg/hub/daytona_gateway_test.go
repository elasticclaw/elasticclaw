package hub

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// gatewaySupervisorStubs tunes the stub binaries the supervisor runs against.
type gatewaySupervisorStubs struct {
	openclaw           string // body of the stub gateway, after it records its run
	healthyAfterRuns   int    // curl reports healthy once this many gateways ran (0 = never)
	healthyAfterProbes int    // ...and curl has been called this many times
	rivalProcess       bool   // pgrep reports another gateway process
	port               int    // gateway port; 0 picks one nothing listens on
}

// gatewaySupervisorCmd prepares the supervisor with stub openclaw, curl, pgrep
// and sleep binaries and a private HOME. Each gateway run appends to dir/runs.
func gatewaySupervisorCmd(t *testing.T, stubs gatewaySupervisorStubs) (cmd *exec.Cmd, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if stubs.port == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick free port: %v", err)
		}
		stubs.port = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	rival := "exit 1"
	if stubs.rivalProcess {
		rival = "exit 0"
	}
	for name, body := range map[string]string{
		"openclaw": `echo run >> "$STUB_DIR/runs"` + "\n" + stubs.openclaw,
		"sleep":    `exit 0`,
		"pgrep":    rival,
		"curl": `echo probe >> "$STUB_DIR/probes"
runs=$(wc -l < "$STUB_DIR/runs" 2>/dev/null || echo 0)
probes=$(wc -l < "$STUB_DIR/probes")
[ "$HEALTHY_AFTER_RUNS" -gt 0 ] && [ "$runs" -ge "$HEALTHY_AFTER_RUNS" ] && [ "$probes" -ge "$HEALTHY_AFTER_PROBES" ]`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	cmd = exec.Command("bash", "-c", daytonaGatewaySupervisorScript(), "openclaw-gateway-supervisor",
		filepath.Join(dir, "supervisor.pid"), filepath.Join(dir, "gateway.log"), strconv.Itoa(stubs.port))
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"HOME="+dir,
		"STUB_DIR="+dir,
		"HEALTHY_AFTER_RUNS="+strconv.Itoa(stubs.healthyAfterRuns),
		"HEALTHY_AFTER_PROBES="+strconv.Itoa(stubs.healthyAfterProbes),
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
// exits 1 (unless stubs says otherwise) and reports what happened.
func runGatewaySupervisor(t *testing.T, stubs gatewaySupervisorStubs) (runs int, log string, exitCode int, dir string) {
	t.Helper()
	if stubs.openclaw == "" {
		stubs.openclaw = "exit 1"
	}
	cmd, dir := gatewaySupervisorCmd(t, stubs)
	exitCode = exitCodeOf(t, cmd.Run())
	data, _ := os.ReadFile(filepath.Join(dir, "runs"))
	logData, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
	return strings.Count(string(data), "run"), string(logData), exitCode, dir
}

func TestBootstrapDaytonaGatewaySupervisor_RestartsUntilBudgetExhausted(t *testing.T) {
	runs, log, exitCode, dir := runGatewaySupervisor(t, gatewaySupervisorStubs{})

	if runs != 6 {
		t.Fatalf("gateway runs = %d, want 6 (first start + 5 restarts)\nlog:\n%s", runs, log)
	}
	if exitCode != 1 {
		t.Fatalf("supervisor exit code = %d, want 1", exitCode)
	}
	assertContains(t, log, "restarting (attempt 5/5) in 80s", "backs off exponentially")
	assertContains(t, log, "restart budget exhausted after 5 attempts", "logs the exhausted budget")
	if _, err := os.Stat(filepath.Join(dir, "supervisor.pid")); !os.IsNotExist(err) {
		t.Fatalf("supervisor pid file still present after exit (stat err: %v)", err)
	}
}

func assertGatewaySupervisorYielded(t *testing.T, runs int, log string, exitCode int) {
	t.Helper()
	if runs != 1 {
		t.Fatalf("gateway runs = %d, want 1: a gateway started elsewhere must not get a rival\nlog:\n%s", runs, log)
	}
	if exitCode != 0 {
		t.Fatalf("supervisor exit code = %d, want 0\nlog:\n%s", exitCode, log)
	}
	assertContains(t, log, "another gateway is already serving port", "logs why it yields")
}

func TestBootstrapDaytonaGatewaySupervisor_YieldsToHealthyGateway(t *testing.T) {
	runs, log, exitCode, _ := runGatewaySupervisor(t, gatewaySupervisorStubs{healthyAfterRuns: 1})

	assertGatewaySupervisorYielded(t, runs, log, exitCode)
}

func TestBootstrapDaytonaGatewaySupervisor_WaitsForStartingRivalProcess(t *testing.T) {
	runs, log, exitCode, _ := runGatewaySupervisor(t, gatewaySupervisorStubs{
		healthyAfterRuns: 1, healthyAfterProbes: 10, rivalProcess: true,
	})

	assertGatewaySupervisorYielded(t, runs, log, exitCode)
}

func TestBootstrapDaytonaGatewaySupervisor_WaitsForStartingRivalListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	runs, log, exitCode, _ := runGatewaySupervisor(t, gatewaySupervisorStubs{
		healthyAfterRuns: 1, healthyAfterProbes: 10, port: l.Addr().(*net.TCPAddr).Port,
	})

	assertGatewaySupervisorYielded(t, runs, log, exitCode)
}

func TestBootstrapDaytonaGatewaySupervisor_RestartsOverRivalThatStaysUnhealthy(t *testing.T) {
	// Nothing is healthy until the supervisor's own second gateway has run.
	runs, log, exitCode, _ := runGatewaySupervisor(t, gatewaySupervisorStubs{healthyAfterRuns: 2, rivalProcess: true})

	if runs != 2 || exitCode != 0 {
		t.Fatalf("gateway runs = %d, exit code = %d; want 2 and 0\nlog:\n%s", runs, exitCode, log)
	}
	assertContains(t, log, "another gateway is not healthy after 30s; restarting anyway", "bounds the wait for a rival")
}

func TestBootstrapDaytonaGatewaySupervisor_SourcesEnvBeforeEachStart(t *testing.T) {
	// The first gateway sees the env written before the supervisor started and
	// rewrites it, as an operator would before killing the gateway.
	cmd, dir := gatewaySupervisorCmd(t, gatewaySupervisorStubs{
		openclaw: `echo "$GATEWAY_MARK" >> "$STUB_DIR/marks"
echo 'export GATEWAY_MARK=updated' > "$HOME/.openclaw/env"
exit 1`,
		healthyAfterRuns: 2,
	})
	if err := os.Mkdir(filepath.Join(dir, ".openclaw"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".openclaw", "env"), []byte("export GATEWAY_MARK=bootstrap\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	if code := exitCodeOf(t, cmd.Run()); code != 0 {
		t.Fatalf("supervisor exit code = %d, want 0", code)
	}

	marks, _ := os.ReadFile(filepath.Join(dir, "marks"))
	if got, want := string(marks), "bootstrap\nupdated\n"; got != want {
		t.Fatalf("gateway env per start = %q, want %q", got, want)
	}
}

func TestBootstrapDaytonaGatewaySupervisor_RivalPatternIgnoresSupervisor(t *testing.T) {
	re := regexp.MustCompile(daytonaGatewayRivalPattern)
	for _, cmdline := range []string{"openclaw-gateway", "openclaw gateway run", "node /usr/local/bin/openclaw gateway run"} {
		if !re.MatchString(cmdline) {
			t.Errorf("rival pattern does not match gateway command line %q", cmdline)
		}
	}
	// pgrep -f sees the supervisor and the bootstrap shell with their scripts
	// inline; matching them would make every restart wait for a phantom rival.
	supervisor := "bash -c " + daytonaGatewaySupervisorScript() + " openclaw-gateway-supervisor /home/daytona/.openclaw/run/openclaw-gateway-supervisor.pid /home/daytona/.openclaw/gateway.log"
	for name, cmdline := range map[string]string{"supervisor": supervisor, "bootstrap": "bash -c " + daytonaStartGatewayCommand()} {
		if loc := re.FindString(cmdline); loc != "" {
			t.Errorf("rival pattern matches the %s command line at %q", name, loc)
		}
	}
}

func TestBootstrapDaytonaGatewaySupervisor_TermStopsGateway(t *testing.T) {
	cmd, dir := gatewaySupervisorCmd(t, gatewaySupervisorStubs{openclaw: `echo $$ > "$STUB_DIR/gateway.pid"; exec /bin/sleep 30`})
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
