package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider implements a local provider for testing
// It runs OpenClaw directly on the local machine in isolated directories
type Provider struct{}

// New creates a new local provider
func New() *Provider {
	return &Provider{}
}

// Info returns provider metadata
func (p *Provider) Info() types.ProviderInfo {
	return types.ProviderInfo{
		Name:         "local",
		Type:         types.ProviderTypeStateful,
		Capabilities: []string{"exec"},
	}
}

// instanceDir returns the directory for an instance
func (p *Provider) instanceDir(instanceID string) (string, error) {
	paths, err := config.GetPaths()
	if err != nil {
		return "", err
	}
	return confinedPath(filepath.Join(paths.StateDir, "local-instances"), instanceID, "instance ID")
}

// Create provisions a new local instance
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	instanceDir, err := p.instanceDir(req.Name)
	if err != nil {
		return nil, err
	}

	// Create instance directory
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create instance directory: %w", err)
	}

	// Create workspace subdirectory
	workspaceDir := filepath.Join(instanceDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace directory: %w", err)
	}

	// Write template files
	for path, content := range req.TemplateFiles {
		fullPath, err := confinedPath(workspaceDir, path, "template path")
		if err != nil {
			return nil, err
		}
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory for %s: %w", path, err)
		}
		if err := os.WriteFile(fullPath, content, 0644); err != nil {
			return nil, fmt.Errorf("failed to write %s: %w", path, err)
		}
	}

	// Write env file
	if len(req.Env) > 0 {
		envFile := filepath.Join(instanceDir, ".env")
		f, err := os.OpenFile(envFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return nil, fmt.Errorf("failed to create env file: %w", err)
		}
		defer f.Close()
		for k, v := range req.Env {
			fmt.Fprintf(f, "%s=%s\n", k, v)
		}
	}

	return &types.Instance{
		Name:      req.Name,
		ID:        req.Name,
		Provider:  "local",
		Status:    types.StatusRunning,
		CreatedAt: time.Now().UTC(),
		ProviderMeta: map[string]string{
			"workspace_dir": workspaceDir,
		},
	}, nil
}

// Status checks current instance state
func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	instanceDir, err := p.instanceDir(instanceID)
	if err != nil {
		return types.StatusUnknown, err
	}

	if _, err := os.Stat(instanceDir); os.IsNotExist(err) {
		return types.StatusNotFound, nil
	}

	return types.StatusRunning, nil
}

// Exec runs a command inside the instance workspace
func (p *Provider) Exec(ctx context.Context, instanceID string, cmdArgs []string) (*types.ExecResult, error) {
	instanceDir, err := p.instanceDir(instanceID)
	if err != nil {
		return nil, err
	}

	workspaceDir := filepath.Join(instanceDir, "workspace")

	if len(cmdArgs) == 0 {
		return nil, fmt.Errorf("no command specified")
	}

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = workspaceDir

	// Load env file if exists
	envFile := filepath.Join(instanceDir, ".env")
	if data, err := os.ReadFile(envFile); err == nil {
		cmd.Env = append(os.Environ(), parseEnvFile(string(data))...)
	}

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
	instanceDir, err := p.instanceDir(instanceID)
	if err != nil {
		return nil, err
	}

	workspaceDir := filepath.Join(instanceDir, "workspace")

	return &types.ConnectInfo{
		Shell: &types.ShellConnect{
			Command: "bash",
			Args:    []string{"-c", fmt.Sprintf("cd %s && exec bash", shellQuote(workspaceDir))},
		},
	}, nil
}

// Stop is a no-op for local provider
func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	return nil
}

// Start is a no-op for local provider
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	return nil
}

// Destroy removes the instance directory
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	instanceDir, err := p.instanceDir(instanceID)
	if err != nil {
		return err
	}

	if keepState {
		// Only remove workspace, keep state
		workspaceDir := filepath.Join(instanceDir, "workspace")
		return os.RemoveAll(workspaceDir)
	}

	return os.RemoveAll(instanceDir)
}

// List returns all local instances
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	paths, err := config.GetPaths()
	if err != nil {
		return nil, err
	}

	instancesDir := filepath.Join(paths.StateDir, "local-instances")
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var instances []*types.Instance
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		instances = append(instances, &types.Instance{
			Name:     entry.Name(),
			ID:       entry.Name(),
			Provider: "local",
			Status:   types.StatusRunning,
		})
	}

	return instances, nil
}

func parseEnvFile(content string) []string {
	var env []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line[0] != '#' {
			env = append(env, line)
		}
	}
	return env
}

func confinedPath(base, path, label string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || clean == "." || filepath.IsAbs(path) {
		return "", fmt.Errorf("invalid %s %q", label, path)
	}
	for _, part := range strings.Split(clean, string(os.PathSeparator)) {
		if part == ".." {
			return "", fmt.Errorf("invalid %s %q", label, path)
		}
	}

	fullPath := filepath.Join(base, clean)
	rel, err := filepath.Rel(base, fullPath)
	if err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", label, path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid %s %q", label, path)
	}

	return fullPath, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
