package bootopt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// VMTestRunner provisions real Replicated VMs and measures actual boot time
// by shelling out to the elasticclaw CLI binary.
//
// This is the ground truth measurement — slower but accurate.
// Each test provisions a real VM, waits for it to come online, measures timing,
// then immediately destroys it.
//
// The hub.yaml used for testing MUST have public_url set to a URL that the
// provisioned VMs can reach. This is typically an ngrok tunnel, Cloudflare
// tunnel, or a publicly accessible server.
//
// Example hub.yaml for testing with ngrok:
//
//	url: http://localhost:8080
//	public_url: https://my-hub.ngrok.io
//	token: hub-token-for-clients
//	claw_token: claw-token-for-vms
//	providers:
//	  replicated:
//	    token: rmi_...       # Replicated CMX API token
//
// Environment variables:
//
//	ELASTICCLAW_HUB_BINARY  - Path to elasticclaw binary (default: "elasticclaw" in PATH)
//	ELASTICCLAW_HUB_CONFIG  - Path to hub.yaml config (default: ~/.elasticclaw/hub.yaml)
//	ELASTICCLAW_BOOTOPT_TEMPLATE - Template name for test claws (default: "base")
//
// Prerequisites:
//
//  1. elasticclaw binary must be built and in PATH (or ELASTICCLAW_HUB_BINARY set)
//  2. hub.yaml must have Replicated CMX provider configured with valid token
//  3. hub.yaml MUST have public_url set so VMs can reach the hub
//  4. Hub server must be running and accessible at public_url
//  5. Template must be pushed to the hub (elasticclaw template push <name>)
//
// Example setup:
//
//	# Build and install
//	go build -o /usr/local/bin/elasticclaw .
//
//	# Configure hub with ngrok public URL
//	cat > ~/.elasticclaw/hub.yaml <<EOF
//	url: http://localhost:8080
//	public_url: https://my-hub.ngrok.io
//	token: $(openssl rand -hex 24)
//	claw_token: $(openssl rand -hex 24)
//	providers:
//	  replicated:
//	    token: $REPLICATED_TOKEN
//	EOF
//
//	# Start hub
//	elasticclaw hub --config ~/.elasticclaw/hub.yaml
//
//	# In another terminal, start ngrok
//	ngrok http 8080
//
//	# Update public_url with ngrok URL, then run tests
//	export ELASTICCLAW_HUB_CONFIG=~/.elasticclaw/hub.yaml
//	export ELASTICCLAW_BOOTOPT_TEMPLATE=base
//	go run ./cmd/bootopt -vm-tests -vm-test-runs 3 -iterations 10
type VMTestRunner struct {
	HubBinary string // Path to elasticclaw binary
	HubConfig string // Path to hub.yaml
	Template  string // Template name for test claws
	Timeout   time.Duration
}

// NewVMTestRunner creates a runner from environment variables.
//
// ELASTICCLAW_HUB_BINARY (optional):
//
//	Path to the elasticclaw binary. If not set, searches PATH for "elasticclaw".
//	The binary must support: claw create, claw list, claw kill
//
// ELASTICCLAW_HUB_CONFIG (optional):
//
//	Path to hub.yaml. The hub.yaml must have:
//	  - providers.replicated.token: <cmx-token>
//	  - public_url: <reachable-from-vms> (CRITICAL — VMs must ping back to this)
//	If not set, uses ~/.elasticclaw/hub.yaml
//
// ELASTICCLAW_BOOTOPT_TEMPLATE (optional):
//
//	Template name for test claws. Default: "base"
//	The template must be pushed to the hub (elasticclaw template push <name>)
func NewVMTestRunner() *VMTestRunner {
	hubBinary := os.Getenv("ELASTICCLAW_HUB_BINARY")
	if hubBinary == "" {
		hubBinary = "elasticclaw"
	}
	hubConfig := os.Getenv("ELASTICCLAW_HUB_CONFIG")
	if hubConfig == "" {
		hubConfig = os.ExpandEnv("$HOME/.elasticclaw/hub.yaml")
	}
	template := os.Getenv("ELASTICCLAW_BOOTOPT_TEMPLATE")
	if template == "" {
		template = "base"
	}
	return NewVMTestRunnerWithConfig(hubBinary, hubConfig, template)
}

// NewVMTestRunnerWithConfig creates a runner with explicit settings.
// hubBinary: path to elasticclaw binary (or "elasticclaw" for PATH lookup)
// hubConfig: path to hub.yaml (must have public_url + replicated.token)
// template: template name (must be pushed to hub)
func NewVMTestRunnerWithConfig(hubBinary, hubConfig, template string) *VMTestRunner {
	if hubBinary == "" {
		hubBinary = "elasticclaw"
	}
	if hubConfig == "" {
		hubConfig = os.ExpandEnv("$HOME/.elasticclaw/hub.yaml")
	}
	if template == "" {
		template = "base"
	}
	return &VMTestRunner{
		HubBinary: hubBinary,
		HubConfig: hubConfig,
		Template:  template,
		Timeout:   5 * time.Minute,
	}
}

// RunVMBootTest provisions a VM and measures time from creation to "online" status.
// Returns on first failure — no retry logic, fail fast.
func (vtr *VMTestRunner) RunVMBootTest(ctx context.Context) (*VMBootResult, error) {
	start := time.Now()
	result := &VMBootResult{
		StartMs: start.UnixMilli(),
		Phases:  make(map[string]int64),
	}

	// Phase 1: VM creation (API call to hub)
	phaseStart := time.Now()
	clawName := fmt.Sprintf("bootopt-%d", start.Unix())
	clawID, err := vtr.createClaw(ctx, clawName)
	if err != nil {
		result.Error = fmt.Sprintf("create claw: %v", err)
		return result, err
	}
	result.ClawID = clawID
	result.Phases["vm_create_api"] = time.Since(phaseStart).Milliseconds()

	// Phase 2: VM provisioning (sandbox startup — Replicated CMX spins the VM)
	phaseStart = time.Now()
	_, err = vtr.waitForStatus(ctx, clawID, "provisioning", 2*time.Minute)
	if err != nil {
		result.Error = fmt.Sprintf("wait provisioning: %v", err)
		vtr.destroyClaw(ctx, clawID)
		return result, err
	}
	result.Phases["vm_provisioning"] = time.Since(phaseStart).Milliseconds()

	// Phase 3: Bootstrap (SSH into VM, run script, install Node/OpenClaw, start bridge)
	phaseStart = time.Now()
	_, err = vtr.waitForStatus(ctx, clawID, "online", vtr.Timeout)
	if err != nil {
		result.Error = fmt.Sprintf("wait online: %v", err)
		vtr.destroyClaw(ctx, clawID)
		return result, err
	}
	result.Phases["bootstrap"] = time.Since(phaseStart).Milliseconds()
	result.TotalMs = time.Since(start).Milliseconds()

	// Phase 4: Cleanup — destroy immediately, we only needed timing
	vtr.destroyClaw(ctx, clawID)

	return result, nil
}

// VMBootResult is the timing result for a single VM boot.
type VMBootResult struct {
	ClawID  string           `json:"claw_id"`
	StartMs int64            `json:"start_ms"`
	TotalMs int64            `json:"total_ms"`
	Phases  map[string]int64 `json:"phases"`
	Error   string           `json:"error,omitempty"`
}

// createClaw provisions a new claw via the elasticclaw CLI.
// Uses: elasticclaw claw create <name> --provider replicated --template <tmpl>
func (vtr *VMTestRunner) createClaw(ctx context.Context, name string) (string, error) {
	args := []string{"claw", "create", name, "--provider", "replicated"}
	if vtr.Template != "" {
		args = append(args, "--template", vtr.Template)
	}
	if vtr.HubConfig != "" {
		args = append(args, "--config", vtr.HubConfig)
	}

	cmd := exec.CommandContext(ctx, vtr.HubBinary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create claw: %w\n%s", err, string(out))
	}

	// Parse claw ID from output. The CLI prints something like:
	// "Created claw bootopt-12345678 (id: 550e8400-e29b-41d4-a716-446655440000)"
	// We look for a UUID-like string (36 chars with 4 dashes)
	output := string(out)
	parts := strings.Fields(output)
	for _, p := range parts {
		p = strings.Trim(p, `()[]{}.,;:"'`)
		if len(p) == 36 && strings.Count(p, "-") == 4 {
			return p, nil
		}
	}
	return "", fmt.Errorf("could not parse claw ID from output: %s", output)
}

// waitForStatus polls the hub until the claw reaches the target status.
// Uses: elasticclaw claw list --json
func (vtr *VMTestRunner) waitForStatus(ctx context.Context, clawID, targetStatus string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, vtr.HubBinary, "claw", "list", "--json")
		if vtr.HubConfig != "" {
			cmd.Args = append(cmd.Args, "--config", vtr.HubConfig)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("list claws: %w\n%s", err, string(out))
		}

		status, err := parseClawStatus(string(out), clawID)
		if err != nil {
			return "", err
		}

		if statusReached(status, targetStatus) {
			return status, nil
		}
		if status == "error" || status == "deleted" {
			return status, fmt.Errorf("claw entered terminal status: %s", status)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return "", fmt.Errorf("timeout waiting for status %s", targetStatus)
}

func statusReached(status, targetStatus string) bool {
	if status == targetStatus {
		return true
	}
	return targetStatus == "provisioning" && (status == "starting" || status == "connected" || status == "online")
}

// destroyClaw kills a claw.
// Uses: elasticclaw claw kill <id>
func (vtr *VMTestRunner) destroyClaw(ctx context.Context, clawID string) error {
	cmd := exec.CommandContext(ctx, vtr.HubBinary, "claw", "kill", clawID)
	if vtr.HubConfig != "" {
		cmd.Args = append(cmd.Args, "--config", vtr.HubConfig)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("destroy claw: %w\n%s", err, string(out))
	}
	return nil
}

// parseClawStatus extracts a claw's status from JSON list output.
func parseClawStatus(jsonOutput, clawID string) (string, error) {
	var claws []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(jsonOutput), &claws); err != nil {
		return "", fmt.Errorf("parse claw list JSON: %w", err)
	}
	for _, claw := range claws {
		if claw.ID == clawID {
			if claw.Status == "" {
				return "", fmt.Errorf("status field not found for claw %s", clawID)
			}
			return claw.Status, nil
		}
	}
	return "", fmt.Errorf("claw %s not found in list output", clawID)
}

// AggregateVMBootResults computes statistics from multiple VM boot tests.
func AggregateVMBootResults(results []*VMBootResult) (mean, median, p95 int64, phaseMeans map[string]int64) {
	var valid []*VMBootResult
	for _, r := range results {
		if r.Error == "" {
			valid = append(valid, r)
		}
	}
	if len(valid) == 0 {
		return 0, 0, 0, nil
	}

	var sum int64
	phaseSums := make(map[string]int64)
	for _, r := range valid {
		sum += r.TotalMs
		for phase, ms := range r.Phases {
			phaseSums[phase] += ms
		}
	}
	mean = sum / int64(len(valid))

	phaseMeans = make(map[string]int64)
	for phase, s := range phaseSums {
		phaseMeans[phase] = s / int64(len(valid))
	}

	// Sort for median and p95
	for i := 0; i < len(valid); i++ {
		for j := i + 1; j < len(valid); j++ {
			if valid[i].TotalMs > valid[j].TotalMs {
				valid[i], valid[j] = valid[j], valid[i]
			}
		}
	}
	mid := len(valid) / 2
	if len(valid)%2 == 0 {
		median = (valid[mid-1].TotalMs + valid[mid].TotalMs) / 2
	} else {
		median = valid[mid].TotalMs
	}

	p95Idx := int(float64(len(valid)) * 0.95)
	if p95Idx >= len(valid) {
		p95Idx = len(valid) - 1
	}
	p95 = valid[p95Idx].TotalMs

	return mean, median, p95, phaseMeans
}
