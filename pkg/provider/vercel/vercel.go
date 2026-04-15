// Package vercel implements an ElasticClaw provider for Vercel Sandbox.
// Vercel Sandbox creates ephemeral Firecracker microVMs accessible via REST API.
// Docs: https://vercel.com/docs/vercel-sandbox
package vercel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL = "https://vercel.com"
	apiBase = "https://api.vercel.com"
)

// Provider implements the Vercel Sandbox provider.
type Provider struct {
	accessToken string
	teamID      string // optional Vercel team ID
	projectID   string // optional Vercel project ID
	httpClient  *http.Client
}

// Config holds Vercel Sandbox provider config (from hub.yaml).
type Config struct {
	AccessToken string // Vercel access token (VERCEL_ACCESS_TOKEN)
	TeamID      string // optional team ID or slug
	ProjectID   string // optional project ID
}

// New creates a new Vercel Sandbox provider.
func New(cfg Config) (*Provider, error) {
	if cfg.AccessToken == "" {
		return nil, fmt.Errorf("vercel: access_token is required (get one at vercel.com/account/tokens)")
	}
	return &Provider{
		accessToken: cfg.AccessToken,
		teamID:      cfg.TeamID,
		projectID:   cfg.ProjectID,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// sandboxResponse is the Vercel API response for sandbox creation.
type sandboxResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// execResponse is the Vercel API response for command execution.
type execResponse struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (p *Provider) do(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}

	url := apiBase + path
	// Append teamId if set
	if p.teamID != "" {
		if strings.Contains(url, "?") {
			url += "&teamId=" + p.teamID
		} else {
			url += "?teamId=" + p.teamID
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// CreateSandbox creates a new Vercel Sandbox and returns its ID.
// env vars are injected at creation time.
func (p *Provider) CreateSandbox(ctx context.Context, name string, env map[string]string) (string, error) {
	reqBody := map[string]interface{}{
		"runtime": "node24",
		"timeout": 24 * 60 * 60 * 1000, // 24h in ms
		"env":     env,
	}
	if p.projectID != "" {
		reqBody["projectId"] = p.projectID
	}

	data, status, err := p.do(ctx, http.MethodPost, "/v1/sandboxes", reqBody)
	if err != nil {
		return "", fmt.Errorf("vercel create sandbox: %w", err)
	}
	if status >= 400 {
		return "", fmt.Errorf("vercel create sandbox: status %d: %s", status, string(data))
	}

	var resp sandboxResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("vercel parse response: %w", err)
	}
	return resp.ID, nil
}

// Exec runs a command in a sandbox and returns stdout + exit code.
func (p *Provider) Exec(ctx context.Context, sandboxID, command string) (string, int, error) {
	reqBody := map[string]string{
		"command": command,
	}

	data, status, err := p.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/sandboxes/%s/exec", sandboxID), reqBody)
	if err != nil {
		return "", -1, fmt.Errorf("vercel exec: %w", err)
	}
	if status >= 400 {
		return "", -1, fmt.Errorf("vercel exec: status %d: %s", status, string(data))
	}

	var resp execResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", -1, fmt.Errorf("vercel parse exec response: %w", err)
	}
	out := resp.Stdout
	if resp.Stderr != "" && resp.ExitCode != 0 {
		out += "\n" + resp.Stderr
	}
	return out, resp.ExitCode, nil
}

// WriteFile writes content to a file inside the sandbox.
func (p *Provider) WriteFile(ctx context.Context, sandboxID, path string, content []byte) error {
	// Escape content for shell echo
	escaped := strings.ReplaceAll(string(content), "'", "'\"'\"'")
	// Use printf to write binary-safe content
	cmd := fmt.Sprintf("mkdir -p $(dirname '%s') && printf '%%s' '%s' > '%s'",
		path, escaped, path)
	_, code, err := p.Exec(ctx, sandboxID, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("write file %s: exit code %d", path, code)
	}
	return nil
}

// DeleteSandbox stops and deletes a sandbox.
func (p *Provider) DeleteSandbox(ctx context.Context, sandboxID string) error {
	_, status, err := p.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/sandboxes/%s", sandboxID), nil)
	if err != nil {
		return err
	}
	if status >= 400 && status != 404 {
		return fmt.Errorf("vercel delete sandbox: status %d", status)
	}
	return nil
}

// GetStatus returns the current status of a sandbox.
func (p *Provider) GetStatus(ctx context.Context, sandboxID string) (string, error) {
	data, status, err := p.do(ctx, http.MethodGet,
		fmt.Sprintf("/v1/sandboxes/%s", sandboxID), nil)
	if err != nil {
		return "", err
	}
	if status == 404 {
		return "not_found", nil
	}
	if status >= 400 {
		return "", fmt.Errorf("vercel get sandbox: status %d", status)
	}

	var resp sandboxResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}
