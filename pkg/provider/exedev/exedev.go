// Package exedev implements an ElasticClaw provider for exe.dev sandboxes.
// exe.dev provides persistent VMs with SSH access, accessible via the SSH CLI.
// Docs: https://exe.dev/docs
package exedev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider implements the exe.dev provider using the SSH CLI.
type Provider struct {
	cfg Config // provider config including SSH key path and default resources
}

// Config holds exe.dev provider config (from hub.yaml).
type Config struct {
	SSHKeyPath string `yaml:"ssh_key_path,omitempty"` // path to SSH private key; if empty, uses default SSH agent
	// Default resource settings for new VMs
	DefaultCPU    int    `yaml:"default_cpu,omitempty"`
	DefaultMemory string `yaml:"default_memory,omitempty"` // e.g. "4GB"
	DefaultDisk   string `yaml:"default_disk,omitempty"`   // e.g. "20GB"
}

// New creates a new exe.dev provider.
func New(cfg Config) (*Provider, error) {
	return &Provider{
		cfg: cfg,
	}, nil
}

// exeVM represents a single VM returned by `ssh exe.dev ls --json`.
type exeVM struct {
	VMName        string `json:"vm_name"`
	SSHDest       string `json:"ssh_dest"`
	Status        string `json:"status"`
	Region        string `json:"region"`
	RegionDisplay string `json:"region_display"`
	HTTPSURL      string `json:"https_url,omitempty"`
}

// exeVMList is the top-level JSON response from `ssh exe.dev ls --json`.
type exeVMList struct {
	VMs []exeVM `json:"vms"`
}

// sshArgs returns the base ssh arguments for control-plane commands (ssh exe.dev ...),
// including identity file if configured.
func (p *Provider) sshArgs() []string {
	var args []string
	if p.cfg.SSHKeyPath != "" {
		args = append(args, "-i", p.cfg.SSHKeyPath)
	}
	args = append(args, "-o", "StrictHostKeyChecking=no", "exe.dev")
	return args
}

// sshVMArgs returns SSH arguments for per-VM commands (ssh <host> ...),
// including identity file and common options if configured.
func (p *Provider) sshVMArgs(host string) []string {
	var args []string
	if p.cfg.SSHKeyPath != "" {
		args = append(args, "-i", p.cfg.SSHKeyPath)
	}
	args = append(args, "-o", "StrictHostKeyChecking=no", host)
	return args
}

// run executes an ssh exe.dev <command> and returns stdout/stderr.
func (p *Provider) run(ctx context.Context, commandParts ...string) ([]byte, error) {
	args := append(p.sshArgs(), commandParts...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return outBuf.Bytes(), fmt.Errorf("ssh exe.dev %s: %w (stderr: %s)", strings.Join(commandParts, " "), err, errBuf.String())
	}
	return outBuf.Bytes(), nil
}

// Info returns provider metadata.
func (p *Provider) Info() types.ProviderInfo {
	return types.ProviderInfo{
		Name:         "exedev",
		Type:         types.ProviderTypeStateful,
		Capabilities: []string{"exec", "ssh"},
	}
}

// Create provisions a new exe.dev VM.
// The instance ID is the vm_name (auto-generated or from ProviderMeta).
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	// Build the new command with optional flags
	args := []string{"new", "--json"}
	if req.Name != "" {
		args = append(args, "--name="+req.Name)
	}
	// Apply default resource settings from provider config
	if p.cfg.DefaultCPU > 0 {
		args = append(args, fmt.Sprintf("--cpu=%d", p.cfg.DefaultCPU))
	}
	if p.cfg.DefaultMemory != "" {
		args = append(args, "--memory="+p.cfg.DefaultMemory)
	}
	if p.cfg.DefaultDisk != "" {
		args = append(args, "--disk="+p.cfg.DefaultDisk)
	}
	// NOTE: env vars are intentionally NOT passed as --env flags to avoid
	// exposing secrets in process listings (ps aux). Env vars are injected
	// during bootstrap via WriteFile/SetupScript instead.

	out, err := p.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("exedev create: %w", err)
	}

	// Parse the JSON response to get vm_name
	var resp struct {
		VMName  string `json:"vm_name"`
		SSHDest string `json:"ssh_dest"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		// Fallback: try to extract vm_name from plain text output
		vmName := extractVMName(string(out))
		if vmName == "" {
			return nil, fmt.Errorf("exedev create: failed to parse response: %s", string(out))
		}
		resp.VMName = vmName
		resp.SSHDest = vmName + ".exe.xyz"
		resp.Status = "running"
	}

	return &types.Instance{
		Name:      req.Name,
		ID:        resp.VMName,
		Provider:  "exedev",
		Status:    types.StatusRunning,
		CreatedAt: time.Now().UTC(),
		ProviderMeta: map[string]string{
			"vm_name":  resp.VMName,
			"ssh_dest": resp.SSHDest,
			"hostname": resp.SSHDest,
		},
	}, nil
}

// Status checks current VM state.
func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	vm, err := p.getVM(ctx, instanceID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return types.StatusNotFound, nil
		}
		return types.StatusUnknown, err
	}
	switch vm.Status {
	case "running":
		return types.StatusRunning, nil
	case "stopped":
		return types.StatusStopped, nil
	case "error":
		return types.StatusError, nil
	default:
		return types.StatusUnknown, nil
	}
}

// Exec runs a command inside the VM via SSH.
func (p *Provider) Exec(ctx context.Context, instanceID string, cmdArgs []string) (*types.ExecResult, error) {
	host := p.vmHost(instanceID)
	if host == "" {
		return nil, fmt.Errorf("exedev exec: cannot determine host for %s", instanceID)
	}

	args := p.sshVMArgs(host)
	args = append(args, cmdArgs...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("exedev exec: %w", err)
		}
	}

	result := &types.ExecResult{
		ExitCode: exitCode,
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
	}
	if exitCode != 0 {
		return result, fmt.Errorf("exedev exec: remote command exited with code %d: %s", exitCode, stderrBuf.String())
	}
	return result, nil
}

// Connect returns SSH connection info.
func (p *Provider) Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error) {
	host := p.vmHost(instanceID)
	return &types.ConnectInfo{
		Shell: &types.ShellConnect{
			Command: "ssh",
			Args:    p.sshVMArgs(host),
		},
	}, nil
}

// Stop is a no-op for exe.dev (VMs are persistent; no hibernate API exposed via SSH CLI).
func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	return nil
}

// Start is a no-op for exe.dev.
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	return nil
}

// Destroy deletes the VM.
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	_, err := p.run(ctx, "rm", instanceID)
	if err != nil {
		// If the VM is already gone, treat as success
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "No such") {
			return nil
		}
		return fmt.Errorf("exedev destroy: %w", err)
	}
	return nil
}

// List returns all VMs managed by this account.
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	out, err := p.run(ctx, "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("exedev list: %w", err)
	}

	var list exeVMList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("exedev list parse: %w", err)
	}

	var instances []*types.Instance
	for _, vm := range list.VMs {
		status := types.StatusUnknown
		switch vm.Status {
		case "running":
			status = types.StatusRunning
		case "stopped":
			status = types.StatusStopped
		case "error":
			status = types.StatusError
		}
		instances = append(instances, &types.Instance{
			Name:     vm.VMName,
			ID:       vm.VMName,
			Provider: "exedev",
			Status:   status,
			ProviderMeta: map[string]string{
				"vm_name":  vm.VMName,
				"ssh_dest": vm.SSHDest,
				"hostname": vm.SSHDest,
			},
		})
	}
	return instances, nil
}

// getVM fetches a single VM by name.
func (p *Provider) getVM(ctx context.Context, vmName string) (*exeVM, error) {
	out, err := p.run(ctx, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var list exeVMList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	for _, vm := range list.VMs {
		if vm.VMName == vmName {
			return &vm, nil
		}
	}
	return nil, fmt.Errorf("vm %s not found", vmName)
}

// vmHost returns the SSH destination for a VM.
func (p *Provider) vmHost(instanceID string) string {
	return instanceID + ".exe.xyz"
}

// SSHKeyPath returns the configured SSH private key path (may be empty).
func (p *Provider) SSHKeyPath() string {
	return p.cfg.SSHKeyPath
}

// extractVMName tries to extract a vm name from plain text output.
func extractVMName(out string) string {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ".exe.xyz") {
			return strings.TrimSuffix(line, ".exe.xyz")
		}
	}
	return ""
}

// shellQuote joins command args for safe shell execution.
func shellQuote(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("'")
		b.WriteString(strings.ReplaceAll(a, "'", "'\"'\"'"))
		b.WriteString("'")
	}
	return b.String()
}

// WriteFile writes content to a file inside the VM via SSH + cat.
// Pipes raw bytes through stdin to avoid shell escaping issues with binary data.
func (p *Provider) WriteFile(ctx context.Context, instanceID string, path string, content []byte) error {
	host := p.vmHost(instanceID)
	if host == "" {
		return fmt.Errorf("exedev writefile: cannot determine host for %s", instanceID)
	}

	args := p.sshVMArgs(host)

	// Ensure parent directory exists
	mkdirArgs := append(args, shellQuote([]string{"mkdir", "-p", "--", filepath.Dir(path)}))
	mkdirCmd := exec.CommandContext(ctx, "ssh", mkdirArgs...)
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("exedev writefile mkdir %s: %w (out: %s)", path, err, string(out))
	}

	// Pipe raw content via stdin to cat on the remote — no escaping needed
	catArgs := append(args, "cat > "+shellQuote([]string{path}))
	cmd := exec.CommandContext(ctx, "ssh", catArgs...)
	cmd.Stdin = bytes.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exedev writefile %s: %w (out: %s)", path, err, string(out))
	}
	return nil
}

// SetupScript runs a setup script on the VM via SSH.
func (p *Provider) SetupScript(ctx context.Context, instanceID string, script string) error {
	host := p.vmHost(instanceID)
	if host == "" {
		return fmt.Errorf("exedev setup: cannot determine host for %s", instanceID)
	}

	args := p.sshVMArgs(host)
	args = append(args, "bash", "-s")

	// Pipe script via stdin to bash on the remote
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exedev setup: %w (out: %s)", err, string(out))
	}
	return nil
}
