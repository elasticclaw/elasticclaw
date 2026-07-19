// Package docker implements an ElasticClaw provider that spawns agent containers
// using the local Docker daemon. This is intended for local development and testing
// without any cloud provider. The hub container must have the Docker socket mounted
// (/var/run/docker.sock) so it can manage sibling containers.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	defaultImage   = cliversion.OpenClawImage
	containerLabel = "elasticclaw.claw"
)

// Config holds docker provider configuration (from hub.yaml providers.docker).
type Config struct {
	// Image is the agent container image. Defaults to the pinned OpenClaw image.
	Image string
	// Network is the Docker network to attach agent containers to so they can
	// reach the hub by its service name (e.g. "elasticclaw-dev").
	Network string
}

// Provider implements the ElasticClaw Provider interface using the Docker CLI.
type Provider struct {
	cfg Config
}

// New creates a new docker provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	return &Provider{cfg: cfg}, nil
}

// Info returns provider metadata.
func (p *Provider) Info() types.ProviderInfo {
	return types.ProviderInfo{
		Name:         "docker",
		Type:         types.ProviderTypeStateful,
		Capabilities: []string{"exec"},
	}
}

// Create launches an agent container. req.Name is used as the container name;
// req.Env is passed as environment variables via -e flags.
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	args := []string{
		"run", "-d",
		"--name", req.Name,
		"--label", containerLabel + "=" + req.Name,
		"--add-host", "host.docker.internal:host-gateway",
	}
	if p.cfg.Image == defaultImage {
		args = append(args, "--entrypoint", "sh")
	}
	if p.cfg.Network != "" {
		args = append(args, "--network", p.cfg.Network)
	}
	for k, v := range req.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, p.cfg.Image)
	if p.cfg.Image == defaultImage {
		args = append(args, "-lc", "trap 'exit 0' TERM INT; while :; do sleep 3600; done")
	}

	out, err := dockerRun(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("docker create: %w", err)
	}

	containerID := strings.TrimSpace(string(out))
	return &types.Instance{
		Name:      req.Name,
		ID:        req.Name, // use name as stable ID; full ID in ProviderMeta
		Provider:  "docker",
		Status:    types.StatusRunning,
		CreatedAt: time.Now().UTC(),
		ProviderMeta: map[string]string{
			"container_id":   containerID,
			"container_name": req.Name,
		},
	}, nil
}

// CopyIn copies content into a running container using a tar stream piped to docker cp.
// dest is an absolute path inside the container (e.g. "/home/claw/workspace/AGENTS.md").
func (p *Provider) CopyIn(ctx context.Context, containerName, dest string, content []byte) error {
	if !strings.HasPrefix(dest, "/") {
		return fmt.Errorf("docker cp: dest must be an absolute path, got %q", dest)
	}

	// Build a single-file tar in memory
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Extract the filename from the destination path
	filename := dest
	if idx := strings.LastIndex(dest, "/"); idx >= 0 {
		filename = dest[idx+1:]
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    filename,
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("docker cp tar header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("docker cp tar write: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("docker cp tar close: %w", err)
	}

	// Determine destination directory
	destDir := "/"
	if idx := strings.LastIndex(dest, "/"); idx > 0 {
		destDir = dest[:idx]
	}

	uid, gid, err := dockerContainerUser(ctx, containerName)
	if err != nil {
		return err
	}

	mkdirCmd := exec.CommandContext(ctx, "docker", "exec", "-u", "0", containerName, "mkdir", "-p", destDir)
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker mkdir -p %s: %w (out: %s)", destDir, err, string(out))
	}
	if destDir != "/" {
		chownDirCmd := exec.CommandContext(ctx, "docker", "exec", "-u", "0", containerName, "chown", uid+":"+gid, destDir)
		if out, err := chownDirCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("docker chown %s: %w (out: %s)", destDir, err, string(out))
		}
		if parentDir := parentPath(destDir); parentDir != "" && parentDir != "/" {
			chownParentCmd := exec.CommandContext(ctx, "docker", "exec", "-u", "0", containerName, "chown", uid+":"+gid, parentDir)
			if out, err := chownParentCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("docker chown %s: %w (out: %s)", parentDir, err, string(out))
			}
		}
	}

	cmd := exec.CommandContext(ctx, "docker", "cp", "-", containerName+":"+destDir)
	cmd.Stdin = &buf
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker cp %s: %w (out: %s)", dest, err, string(out))
	}
	chownFileCmd := exec.CommandContext(ctx, "docker", "exec", "-u", "0", containerName, "chown", uid+":"+gid, dest)
	if out, err := chownFileCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker chown %s: %w (out: %s)", dest, err, string(out))
	}
	return nil
}

func parentPath(path string) string {
	path = strings.TrimRight(path, "/")
	if path == "" || path == "/" {
		return ""
	}
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "/"
	}
	return path[:idx]
}

func dockerContainerUser(ctx context.Context, containerName string) (string, string, error) {
	uidCmd := exec.CommandContext(ctx, "docker", "exec", containerName, "id", "-u")
	uidOut, err := uidCmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("docker id -u: %w (out: %s)", err, string(uidOut))
	}
	gidCmd := exec.CommandContext(ctx, "docker", "exec", containerName, "id", "-g")
	gidOut, err := gidCmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("docker id -g: %w (out: %s)", err, string(gidOut))
	}
	uid := strings.TrimSpace(string(uidOut))
	gid := strings.TrimSpace(string(gidOut))
	if uid == "" || gid == "" {
		return "", "", fmt.Errorf("docker user lookup returned empty uid/gid")
	}
	return uid, gid, nil
}

// Exec runs a command inside a running container.
func (p *Provider) Exec(ctx context.Context, instanceID string, cmdArgs []string) (*types.ExecResult, error) {
	args := append([]string{"exec", instanceID}, cmdArgs...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
		} else {
			return nil, fmt.Errorf("docker exec: %w", err)
		}
	}
	res := &types.ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
	if exitCode != 0 {
		return res, fmt.Errorf("docker exec: command exited %d: %s", exitCode, stderr.String())
	}
	return res, nil
}

// HomeDir returns the default user's home directory inside the running container.
func (p *Provider) HomeDir(ctx context.Context, instanceID string) (string, error) {
	out, err := dockerRun(ctx, "exec", instanceID, "sh", "-lc", `printf '%s' "$HOME"`)
	if err != nil {
		return "", fmt.Errorf("docker home dir: %w", err)
	}
	home := strings.TrimSpace(string(out))
	if home == "" || !strings.HasPrefix(home, "/") {
		return "", fmt.Errorf("docker home dir returned invalid path %q", home)
	}
	return home, nil
}

// Connect returns a shell command to exec into the container.
func (p *Provider) Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error) {
	return &types.ConnectInfo{
		Shell: &types.ShellConnect{
			Command: "docker",
			Args:    []string{"exec", "-it", instanceID, "bash"},
		},
	}, nil
}

// Status returns the current container state.
func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	out, err := dockerRun(ctx, "inspect", "-f", "{{.State.Status}}", instanceID)
	if err != nil {
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "not found") {
			return types.StatusNotFound, nil
		}
		return types.StatusUnknown, fmt.Errorf("docker inspect: %w", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "running":
		return types.StatusRunning, nil
	case "exited", "dead":
		return types.StatusStopped, nil
	case "created", "paused":
		return types.StatusStarting, nil
	default:
		return types.StatusUnknown, nil
	}
}

// Stop stops a running container.
func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	_, err := dockerRun(ctx, "stop", instanceID)
	return err
}

// Start restarts a stopped container.
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	_, err := dockerRun(ctx, "start", instanceID)
	return err
}

// Destroy removes the container (forcefully if still running).
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	_, err := dockerRun(ctx, "rm", "-f", instanceID)
	if err != nil {
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "not found") {
			return nil // already gone
		}
		if strings.Contains(err.Error(), "removal of container") && strings.Contains(err.Error(), "is already in progress") {
			return nil
		}
		return fmt.Errorf("docker rm: %w", err)
	}
	return nil
}

// List returns all containers managed by this provider (labeled with elasticclaw.claw).
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	out, err := dockerRun(ctx, "ps", "-a",
		"--filter", "label="+containerLabel,
		"--format", "{{.Names}}\t{{.Status}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	var instances []*types.Instance
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		name := parts[0]
		status := types.StatusUnknown
		if len(parts) == 2 {
			st := strings.ToLower(parts[1])
			switch {
			case strings.HasPrefix(st, "up"):
				status = types.StatusRunning
			case strings.HasPrefix(st, "exited"):
				status = types.StatusStopped
			}
		}
		instances = append(instances, &types.Instance{
			Name:     name,
			ID:       name,
			Provider: "docker",
			Status:   status,
		})
	}
	return instances, nil
}

// dockerRun executes a docker CLI command and returns its stdout.
func dockerRun(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("docker %s: %w (stderr: %s)", strings.Join(args[:min(len(args), 2)], " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}
