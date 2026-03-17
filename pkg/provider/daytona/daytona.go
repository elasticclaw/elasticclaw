package daytona

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider implements the Daytona provider
type Provider struct {
	organization string
}

// New creates a new Daytona provider
func New(config map[string]interface{}) *Provider {
	org := ""
	if config != nil {
		if o, ok := config["organization"].(string); ok {
			org = o
		}
	}
	return &Provider{organization: org}
}

// Info returns provider metadata
func (p *Provider) Info() types.ProviderInfo {
	return types.ProviderInfo{
		Name:         "daytona",
		Type:         types.ProviderTypeEphemeral,
		Capabilities: []string{"exec", "snapshot"},
	}
}

// Create provisions a new instance
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	// Build daytona create command
	args := []string{"create", "--name", req.Name}

	if req.FromImage != "" {
		args = append(args, "--image", req.FromImage)
	}

	// Execute daytona create
	cmd := exec.CommandContext(ctx, "daytona", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("daytona create failed: %w\n%s", err, string(output))
	}

	// Parse workspace ID from output
	// For now, use the name as ID
	workspaceID := req.Name

	// Inject template files via exec
	for path, content := range req.TemplateFiles {
		if err := p.writeFile(ctx, workspaceID, path, content); err != nil {
			// Cleanup on failure
			p.Destroy(ctx, workspaceID, false)
			return nil, fmt.Errorf("failed to inject template file %s: %w", path, err)
		}
	}

	// Set environment variables
	for key, value := range req.Env {
		if err := p.setEnv(ctx, workspaceID, key, value); err != nil {
			p.Destroy(ctx, workspaceID, false)
			return nil, fmt.Errorf("failed to set env %s: %w", key, err)
		}
	}

	return &types.Instance{
		Name:     req.Name,
		ID:       workspaceID,
		Provider: "daytona",
		Status:   types.StatusRunning,
		ProviderMeta: map[string]string{
			"workspace_id": workspaceID,
		},
	}, nil
}

// Status checks current instance state
func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	cmd := exec.CommandContext(ctx, "daytona", "info", instanceID, "--format", "json")
	output, err := cmd.Output()
	if err != nil {
		if strings.Contains(string(output), "not found") {
			return types.StatusNotFound, nil
		}
		return types.StatusUnknown, err
	}

	var info struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return types.StatusUnknown, err
	}

	switch info.State {
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

// Exec runs a command inside the instance
func (p *Provider) Exec(ctx context.Context, instanceID string, cmdArgs []string) (*types.ExecResult, error) {
	args := append([]string{"exec", instanceID, "--"}, cmdArgs...)
	cmd := exec.CommandContext(ctx, "daytona", args...)
	output, err := cmd.CombinedOutput()

	result := &types.ExecResult{
		Stdout: string(output),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}

	return result, nil
}

// Connect returns connection info
func (p *Provider) Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error) {
	return &types.ConnectInfo{
		Shell: &types.ShellConnect{
			Command: "daytona",
			Args:    []string{"ssh", instanceID},
		},
	}, nil
}

// Stop pauses/hibernates the instance
func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	cmd := exec.CommandContext(ctx, "daytona", "stop", instanceID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("daytona stop failed: %w\n%s", err, string(output))
	}
	return nil
}

// Start resumes a stopped instance
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	cmd := exec.CommandContext(ctx, "daytona", "start", instanceID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("daytona start failed: %w\n%s", err, string(output))
	}
	return nil
}

// Destroy tears down the instance
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	args := []string{"delete", instanceID, "-y"}
	cmd := exec.CommandContext(ctx, "daytona", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("daytona delete failed: %w\n%s", err, string(output))
	}
	return nil
}

// List returns all instances managed by this provider
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	cmd := exec.CommandContext(ctx, "daytona", "list", "--format", "json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("daytona list failed: %w", err)
	}

	var workspaces []struct {
		Name  string `json:"name"`
		ID    string `json:"id"`
		State string `json:"state"`
	}

	if err := json.Unmarshal(output, &workspaces); err != nil {
		return nil, fmt.Errorf("failed to parse daytona output: %w", err)
	}

	var instances []*types.Instance
	for _, ws := range workspaces {
		status := types.StatusUnknown
		switch ws.State {
		case "running":
			status = types.StatusRunning
		case "stopped":
			status = types.StatusStopped
		}
		instances = append(instances, &types.Instance{
			Name:     ws.Name,
			ID:       ws.ID,
			Provider: "daytona",
			Status:   status,
		})
	}

	return instances, nil
}

// Helper functions

func (p *Provider) writeFile(ctx context.Context, instanceID, path string, content []byte) error {
	// Use echo and base64 to write file content
	encoded := string(content) // In production, use base64 encoding
	cmd := fmt.Sprintf("echo '%s' > %s", strings.ReplaceAll(encoded, "'", "'\\''"), path)
	_, err := p.Exec(ctx, instanceID, []string{"sh", "-c", cmd})
	return err
}

func (p *Provider) setEnv(ctx context.Context, instanceID, key, value string) error {
	// Append to ~/.bashrc or similar
	cmd := fmt.Sprintf("echo 'export %s=\"%s\"' >> ~/.bashrc", key, value)
	_, err := p.Exec(ctx, instanceID, []string{"sh", "-c", cmd})
	return err
}
