package daytona

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	daytonaerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	daytonaopts "github.com/daytona/clients/sdk-go/pkg/options"
	daytonatypes "github.com/daytona/clients/sdk-go/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider implements the Daytona provider using the official SDK
type Provider struct {
	client *daytona.Client
	apiKey string
}

var shellEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func buildOpenClawEnvFile(env map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(env))
	for key := range env {
		if !shellEnvNameRE.MatchString(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	envLines := make([]string, 0, len(keys))
	for _, key := range keys {
		escapedValue := strings.ReplaceAll(env[key], "'", "'\"'\"'")
		envLines = append(envLines, fmt.Sprintf("export %s='%s'", key, escapedValue))
	}
	return []byte(strings.Join(envLines, "\n")), nil
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
		Capabilities: []string{"exec", "snapshot", "preview"},
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

	providerMeta := map[string]string{"sandbox_id": sandbox.ID}
	for _, port := range req.PreviewPorts {
		preview, err := sandbox.GetSignedPreviewLink(ctx, port, 24*60*60)
		if err != nil {
			_ = sandbox.Delete(context.Background())
			return nil, fmt.Errorf("failed to expose preview port %d: %w", port, err)
		}
		providerMeta[fmt.Sprintf("preview_url_%d", port)] = preview.URL
	}

	return &types.Instance{
		Name:         req.Name,
		ID:           sandbox.ID,
		Provider:     "daytona",
		Status:       types.StatusRunning,
		CreatedAt:    time.Now().UTC(),
		ProviderMeta: providerMeta,
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
	sandbox, err := p.client.Get(ctx, instanceID)
	if err != nil {
		if isDaytonaNotFound(err) {
			return types.StatusNotFound, nil
		}
		return types.StatusUnknown, err
	}

	return instanceStatus(sandbox.State), nil
}

func instanceStatus(state daytona.SandboxState) types.InstanceStatus {
	switch state {
	case daytona.SandboxStateStarted:
		return types.StatusRunning
	case daytona.SandboxStateStopped, daytona.SandboxStatePaused, daytona.SandboxStateArchived:
		return types.StatusStopped
	case daytona.SandboxStateError, daytona.SandboxStateBuildFailed:
		return types.StatusError
	case daytona.SandboxStateCreating,
		daytona.SandboxStateRestoring,
		daytona.SandboxStateStarting,
		daytona.SandboxStatePendingBuild,
		daytona.SandboxStateBuildingSnapshot,
		daytona.SandboxStatePullingSnapshot,
		daytona.SandboxStateResuming:
		return types.StatusStarting
	default:
		return types.StatusUnknown
	}
}

func isDaytonaNotFound(err error) bool {
	var notFoundErr *daytonaerrors.DaytonaNotFoundError
	return errors.As(err, &notFoundErr)
}

// Exec runs a command inside the sandbox
func (p *Provider) Exec(ctx context.Context, instanceID string, cmdArgs []string) (*types.ExecResult, error) {
	return p.ExecWithTimeout(ctx, instanceID, cmdArgs, 60*time.Second)
}

// ExecWithTimeout runs a command with an explicit timeout. Use for long-running
// operations like package installs that exceed the default 60s SDK HTTP timeout.
func (p *Provider) ExecWithTimeout(ctx context.Context, instanceID string, cmdArgs []string, timeout time.Duration) (*types.ExecResult, error) {
	sandbox, err := p.client.Get(ctx, instanceID)
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

// IsTransientExecError reports whether a failed Daytona command is safe to
// retry. Keepalive commands are idempotent, but permanent failures such as a
// missing sandbox should still fail immediately.
func IsTransientExecError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var timeoutErr *daytonaerrors.DaytonaTimeoutError
	if errors.As(err, &timeoutErr) {
		return true
	}
	var daytonaErr *daytonaerrors.DaytonaError
	if errors.As(err, &daytonaErr) {
		return daytonaErr.StatusCode == http.StatusRequestTimeout ||
			daytonaErr.StatusCode == http.StatusTooManyRequests ||
			daytonaErr.StatusCode >= http.StatusInternalServerError
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout") || strings.Contains(message, "timed out")
}

func (p *Provider) EnsureSession(ctx context.Context, instanceID, sessionID string) error {
	sandbox, err := p.client.Get(ctx, instanceID)
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
	sandbox, err := p.client.Get(ctx, instanceID)
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
	sandbox, err := p.client.Get(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	return sandbox.Stop(ctx)
}

// Start resumes a stopped sandbox
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	sandbox, err := p.client.Get(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	return sandbox.Start(ctx)
}

// Destroy deletes the sandbox
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	sandbox, err := p.client.Get(ctx, instanceID)
	if err != nil {
		if isDaytonaNotFound(err) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	return sandbox.Delete(ctx)
}

// List returns all sandboxes
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	var instances []*types.Instance
	iterator := p.client.List(ctx, nil)
	for iterator.Next() {
		sandbox := iterator.Value()
		instances = append(instances, &types.Instance{
			Name:     sandbox.Name,
			ID:       sandbox.ID,
			Provider: "daytona",
			Status:   instanceStatus(sandbox.State),
		})
	}
	if err := iterator.Err(); err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}

	return instances, nil
}

// ConfigureOpenClaw configures OpenClaw with necessary API keys and settings
func (p *Provider) ConfigureOpenClaw(ctx context.Context, instanceID string, env map[string]string) error {
	sandbox, err := p.client.Get(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("failed to find sandbox: %w", err)
	}

	// Create openclaw config directory
	response, err := sandbox.Process.ExecuteCommand(ctx, "bash -c 'mkdir -p /home/daytona/.openclaw'")
	if err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	if response.ExitCode != 0 {
		return fmt.Errorf("failed to create config dir: command exited with status %d", response.ExitCode)
	}

	// Write environment to a file that gets sourced
	if len(env) > 0 {
		envContent, err := buildOpenClawEnvFile(env)
		if err != nil {
			return err
		}

		// Write env file
		err = sandbox.FileSystem.UploadFile(ctx, envContent, "/home/daytona/.openclaw/env")
		if err != nil {
			return fmt.Errorf("failed to write env file: %w", err)
		}

		// Source it from bashrc
		response, err = sandbox.Process.ExecuteCommand(ctx,
			"bash -c 'grep -q openclaw/env /home/daytona/.bashrc || echo \"source ~/.openclaw/env\" >> /home/daytona/.bashrc'")
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
	sandbox, err := p.client.Get(ctx, instanceID)
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
	script := fmt.Sprintf("cd %s && { source ~/.openclaw/env 2>/dev/null || true; setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null & }", shellQuote(workdir))
	return fmt.Sprintf("bash -c %s", shellQuote(script))
}
