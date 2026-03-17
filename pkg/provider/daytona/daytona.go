package daytona

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	daytonatypes "github.com/daytonaio/daytona/libs/sdk-go/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider implements the Daytona provider using the official SDK
type Provider struct {
	client *daytona.Client
	apiKey string
}

// New creates a new Daytona provider
func New(config map[string]interface{}) (*Provider, error) {
	apiKey := os.Getenv("DAYTONA_API_KEY")
	if config != nil {
		if key, ok := config["api_key"].(string); ok && key != "" {
			apiKey = key
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("DAYTONA_API_KEY not set - get one at https://app.daytona.io/dashboard/keys")
	}

	// Create client with config
	cfg := &daytonatypes.DaytonaConfig{
		APIKey: apiKey,
	}

	// Check for custom API URL
	if apiURL := os.Getenv("DAYTONA_API_URL"); apiURL != "" {
		cfg.APIUrl = apiURL
	}

	// Check for target region
	if target := os.Getenv("DAYTONA_TARGET"); target != "" {
		cfg.Target = target
	}

	client, err := daytona.NewClientWithConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Daytona client: %w", err)
	}

	return &Provider{
		client: client,
		apiKey: apiKey,
	}, nil
}

// Info returns provider metadata
func (p *Provider) Info() types.ProviderInfo {
	return types.ProviderInfo{
		Name:         "daytona",
		Type:         types.ProviderTypeEphemeral,
		Capabilities: []string{"exec", "snapshot"},
	}
}

// Create provisions a new sandbox
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	// Create sandbox params - use SnapshotParams for default snapshot-based creation
	params := daytonatypes.SnapshotParams{
		SandboxBaseParams: daytonatypes.SandboxBaseParams{
			Name:    req.Name,
			EnvVars: req.Env,
		},
	}

	// Create the sandbox
	sandbox, err := p.client.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", err)
	}

	// Inject template files
	for path, content := range req.TemplateFiles {
		// Ensure directory exists
		dir := getDir(path)
		if dir != "" && dir != "." {
			sandbox.FileSystem.CreateFolder(ctx, dir)
		}
		
		// UploadFile accepts []byte or string (path) as source
		err := sandbox.FileSystem.UploadFile(ctx, content, path)
		if err != nil {
			// Try to clean up on failure
			sandbox.Delete(ctx)
			return nil, fmt.Errorf("failed to write file %s: %w", path, err)
		}
	}

	return &types.Instance{
		Name:      req.Name,
		ID:        sandbox.ID,
		Provider:  "daytona",
		Status:    types.StatusRunning,
		CreatedAt: time.Now().UTC(),
		ProviderMeta: map[string]string{
			"sandbox_id": sandbox.ID,
		},
	}, nil
}

func getDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}

// Status checks current sandbox state
func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return types.StatusNotFound, nil
		}
		return types.StatusUnknown, err
	}

	switch sandbox.State {
	case "started", "running":
		return types.StatusRunning, nil
	case "stopped":
		return types.StatusStopped, nil
	case "error":
		return types.StatusError, nil
	case "pending", "starting":
		return types.StatusStarting, nil
	default:
		return types.StatusUnknown, nil
	}
}

// Exec runs a command inside the sandbox
func (p *Provider) Exec(ctx context.Context, instanceID string, cmdArgs []string) (*types.ExecResult, error) {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to find sandbox: %w", err)
	}

	// Join command args into a single command string
	cmd := strings.Join(cmdArgs, " ")

	response, err := sandbox.Process.ExecuteCommand(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to execute command: %w", err)
	}

	return &types.ExecResult{
		ExitCode: response.ExitCode,
		Stdout:   response.Result,
	}, nil
}

// Connect returns connection info
func (p *Provider) Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error) {
	// Daytona sandboxes are accessed via SDK/API
	return &types.ConnectInfo{
		Shell: &types.ShellConnect{
			Command: "daytona",
			Args:    []string{"ssh", instanceID},
		},
	}, nil
}

// Stop pauses the sandbox
func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	return sandbox.Stop(ctx)
}

// Start resumes a stopped sandbox
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	return sandbox.Start(ctx)
}

// Destroy deletes the sandbox
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil // Already gone
		}
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	return sandbox.Delete(ctx)
}

// List returns all sandboxes
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	result, err := p.client.List(ctx, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}

	var instances []*types.Instance
	for _, sandbox := range result.Items {
		status := types.StatusUnknown
		switch sandbox.State {
		case "started", "running":
			status = types.StatusRunning
		case "stopped":
			status = types.StatusStopped
		case "error":
			status = types.StatusError
		}

		instances = append(instances, &types.Instance{
			Name:     sandbox.ID,
			ID:       sandbox.ID,
			Provider: "daytona",
			Status:   status,
		})
	}

	return instances, nil
}

// InstallOpenClaw installs OpenClaw in a sandbox
func (p *Provider) InstallOpenClaw(ctx context.Context, instanceID string) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	// Install Node.js and OpenClaw
	commands := []string{
		"curl -fsSL https://deb.nodesource.com/setup_22.x | bash -",
		"apt-get install -y nodejs",
		"npm install -g openclaw",
	}

	for _, cmd := range commands {
		response, err := sandbox.Process.ExecuteCommand(ctx, cmd)
		if err != nil {
			return fmt.Errorf("failed to run %q: %w", cmd, err)
		}
		if response.ExitCode != 0 {
			return fmt.Errorf("command %q failed: %s", cmd, response.Result)
		}
	}

	return nil
}
