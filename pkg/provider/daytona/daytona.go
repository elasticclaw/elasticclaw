package daytona

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	daytonaopts "github.com/daytonaio/daytona/libs/sdk-go/pkg/options"
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
	// Create sandbox params - use SnapshotParams for default snapshot-based creation.
	// req.FromImage maps to the Daytona snapshot name (e.g. "daytona-medium").
	params := daytonatypes.SnapshotParams{
		SandboxBaseParams: daytonatypes.SandboxBaseParams{
			Name:    req.Name,
			EnvVars: req.Env,
		},
		Snapshot: req.FromImage, // snapshot name — empty string uses Daytona default
	}

	// Create the sandbox
	sandbox, err := p.client.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", err)
	}

	// Inject template files
	for path, content := range req.TemplateFiles {
		// Skip empty files (like .gitkeep)
		if len(content) == 0 {
			continue
		}

		// Use absolute paths
		absPath := path
		if !strings.HasPrefix(path, "/") {
			absPath = "/home/daytona/" + path
		}

		// Ensure directory exists
		dir := getDir(absPath)
		if dir != "" && dir != "." {
			sandbox.FileSystem.CreateFolder(ctx, dir)
		}

		// UploadFile accepts []byte or string (path) as source
		err := sandbox.FileSystem.UploadFile(ctx, content, absPath)
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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
	return p.ExecWithTimeout(ctx, instanceID, cmdArgs, 60*time.Second)
}

// ExecWithTimeout runs a command with an explicit timeout. Use for long-running
// operations like package installs that exceed the default 60s SDK HTTP timeout.
func (p *Provider) ExecWithTimeout(ctx context.Context, instanceID string, cmdArgs []string, timeout time.Duration) (*types.ExecResult, error) {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to find sandbox: %w", err)
	}

	// Join command args and wrap in bash -c for proper shell handling
	cmd := strings.Join(cmdArgs, " ")
	// Escape single quotes in the command
	escapedCmd := strings.ReplaceAll(cmd, "'", "'\"'\"'")
	wrappedCmd := fmt.Sprintf("bash -c '%s'", escapedCmd)

	// Use a context with the timeout so the HTTP client respects it.
	// The SDK's default HTTP client has a 60s timeout; context overrides it.
	execCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	response, err := sandbox.Process.ExecuteCommand(execCtx, wrappedCmd, daytonaopts.WithExecuteTimeout(timeout))
	if err != nil {
		return nil, fmt.Errorf("failed to execute command: %w", err)
	}

	return &types.ExecResult{
		ExitCode: response.ExitCode,
		Stdout:   response.Result,
	}, nil
}

func (p *Provider) EnsureSession(ctx context.Context, instanceID, sessionID string) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}
	if _, err := sandbox.Process.GetSession(ctx, sessionID); err == nil {
		return nil
	}
	if err := sandbox.Process.CreateSession(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (p *Provider) ExecSessionAsync(ctx context.Context, instanceID, sessionID, command string) (string, error) {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return "", fmt.Errorf("failed to find sandbox: %w", err)
	}
	result, err := sandbox.Process.ExecuteSessionCommand(ctx, sessionID, command, true, true)
	if err != nil {
		return "", fmt.Errorf("failed to execute async session command: %w", err)
	}
	if id, ok := result["id"].(string); ok {
		return id, nil
	}
	return "", nil
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

// ConfigureOpenClaw configures OpenClaw with necessary API keys and settings
func (p *Provider) ConfigureOpenClaw(ctx context.Context, instanceID string, env map[string]string) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	// Create openclaw config directory
	response, err := sandbox.Process.ExecuteCommand(ctx, "bash -c 'mkdir -p ~/.openclaw'")
	if err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}

	// Write environment to a file that gets sourced
	if len(env) > 0 {
		var envLines []string
		for k, v := range env {
			// Escape for shell
			escapedV := strings.ReplaceAll(v, "'", "'\"'\"'")
			envLines = append(envLines, fmt.Sprintf("export %s='%s'", k, escapedV))
		}
		envContent := strings.Join(envLines, "\n")

		// Write env file
		err = sandbox.FileSystem.UploadFile(ctx, []byte(envContent), "/home/daytona/.openclaw/env")
		if err != nil {
			return fmt.Errorf("failed to write env file: %w", err)
		}

		// Source it from bashrc
		response, err = sandbox.Process.ExecuteCommand(ctx,
			"bash -c 'grep -q openclaw/env ~/.bashrc || echo \"source ~/.openclaw/env\" >> ~/.bashrc'")
		if err != nil || response.ExitCode != 0 {
			// Non-fatal
		}
	}

	return nil
}

// StartOpenClaw starts the OpenClaw gateway in the sandbox.
// Note: this is a standalone helper; the main bootstrap path in
// bootstrapDaytona (pkg/hub/server.go) handles full two-phase startup.
func (p *Provider) StartOpenClaw(ctx context.Context, instanceID string, workdir string) error {
	sandbox, err := p.client.FindOne(ctx, &instanceID, nil)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	// Use gateway run (foreground mode) with setsid/nohup so the gateway
	// stays alive after the exec session ends.  gateway start is for
	// systemd/launchd service installation and is not appropriate here.
	cmd := buildStartOpenClawCommand(workdir)
	response, err := sandbox.Process.ExecuteCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("failed to start gateway: %w", err)
	}
	if response.ExitCode != 0 {
		return fmt.Errorf("gateway start failed: %s", response.Result)
	}

	return nil
}

func buildStartOpenClawCommand(workdir string) string {
	script := fmt.Sprintf("cd %s && source ~/.openclaw/env 2>/dev/null; setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null &", shellQuote(workdir))
	return fmt.Sprintf("bash -c %s", shellQuote(script))
}
