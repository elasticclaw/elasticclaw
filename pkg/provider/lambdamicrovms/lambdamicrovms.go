package lambdamicrovms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	DefaultBridgePort             = 8080
	DefaultTokenExpirationMinutes = 30
	DefaultMaximumDurationSeconds = 28800
)

type Config struct {
	Region                   string
	Profile                  string
	ImageIdentifier          string
	ImageVersion             string
	ExecutionRoleARN         string
	IngressNetworkConnectors []string
	EgressNetworkConnectors  []string
	IdleMaxDurationSeconds   int
	SuspendedDurationSeconds int
	AutoResume               *bool
	MaximumDurationSeconds   int
	BridgePort               int
	TokenExpirationMinutes   int
}

type Provider struct {
	cfg  Config
	http *http.Client
}

type runHookPayload struct {
	Version       int               `json:"version"`
	Name          string            `json:"name"`
	Env           map[string]string `json:"env,omitempty"`
	TemplateFiles map[string]string `json:"templateFiles,omitempty"`
	StateMount    string            `json:"stateMount,omitempty"`
}

type runMicroVMResponse struct {
	MicroVMID string `json:"microvmId"`
	State     string `json:"state"`
	Endpoint  string `json:"endpoint"`
}

type getMicroVMResponse struct {
	MicroVMID string `json:"microvmId"`
	State     string `json:"state"`
	Endpoint  string `json:"endpoint"`
}

type authTokenResponse struct {
	AuthToken map[string]string `json:"authToken"`
}

type execRequest struct {
	Command []string `json:"command"`
}

type writeFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func New(cfg Config) (*Provider, error) {
	cfg.ImageIdentifier = strings.TrimSpace(cfg.ImageIdentifier)
	if cfg.ImageIdentifier == "" {
		return nil, fmt.Errorf("lambda microvms provider requires image_identifier")
	}
	if cfg.BridgePort == 0 {
		cfg.BridgePort = DefaultBridgePort
	}
	if cfg.BridgePort < 1 || cfg.BridgePort > 65535 {
		return nil, fmt.Errorf("lambda microvms bridge_port must be between 1 and 65535")
	}
	if cfg.TokenExpirationMinutes == 0 {
		cfg.TokenExpirationMinutes = DefaultTokenExpirationMinutes
	}
	if cfg.TokenExpirationMinutes < 1 || cfg.TokenExpirationMinutes > 60 {
		return nil, fmt.Errorf("lambda microvms auth_token_expiration_minutes must be between 1 and 60")
	}
	if cfg.MaximumDurationSeconds == 0 {
		cfg.MaximumDurationSeconds = DefaultMaximumDurationSeconds
	}
	if cfg.MaximumDurationSeconds < 1 || cfg.MaximumDurationSeconds > DefaultMaximumDurationSeconds {
		return nil, fmt.Errorf("lambda microvms maximum_duration_seconds must be between 1 and %d", DefaultMaximumDurationSeconds)
	}
	if cfg.IdleMaxDurationSeconds > 0 || cfg.SuspendedDurationSeconds > 0 || cfg.AutoResume != nil {
		if cfg.IdleMaxDurationSeconds < 60 || cfg.AutoResume == nil {
			return nil, fmt.Errorf("lambda microvms idle policy requires idle_max_duration_seconds >= 60 and auto_resume")
		}
	}
	return &Provider{cfg: cfg, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (p *Provider) Info() types.ProviderInfo {
	return types.ProviderInfo{
		Name:         "lambda-microvms",
		Type:         types.ProviderTypeStateful,
		Capabilities: []string{"alpha", "stateful", "https-bridge"},
	}
}

func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	initializationPayload, err := buildInitializationPayload(req)
	if err != nil {
		return nil, err
	}
	runPayload, err := json.Marshal(runHookPayload{Version: 1, Name: req.Name})
	if err != nil {
		return nil, fmt.Errorf("marshal run hook payload: %w", err)
	}
	payloadArg, cleanup, err := writeRunHookPayloadFile(string(runPayload))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	args, err := p.runMicroVMArgs(payloadArg)
	if err != nil {
		return nil, err
	}
	out, err := p.aws(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("run microvm: %w", err)
	}
	var resp runMicroVMResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse run microvm response: %w", err)
	}
	if resp.MicroVMID == "" {
		return nil, fmt.Errorf("run microvm response missing microvmId")
	}
	if err := p.initialize(ctx, resp.MicroVMID, resp.Endpoint, initializationPayload); err != nil {
		_, _ = p.aws(context.Background(), "lambda-microvms", "terminate-microvm", "--microvm-identifier", resp.MicroVMID)
		return nil, fmt.Errorf("initialize microvm: %w", err)
	}
	return &types.Instance{
		Name:      req.Name,
		ID:        resp.MicroVMID,
		Provider:  "lambda-microvms",
		Status:    mapState(resp.State),
		CreatedAt: time.Now().UTC(),
		ProviderMeta: map[string]string{
			"endpoint": resp.Endpoint,
		},
	}, nil
}

func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	vm, err := p.get(ctx, instanceID)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			return types.StatusNotFound, nil
		}
		return types.StatusUnknown, err
	}
	return mapState(vm.State), nil
}

func (p *Provider) Exec(ctx context.Context, instanceID string, cmd []string) (*types.ExecResult, error) {
	var result types.ExecResult
	if err := p.bridgeJSON(ctx, instanceID, http.MethodPost, "/elasticclaw/v1/exec", execRequest{Command: cmd}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (p *Provider) WriteFile(ctx context.Context, instanceID string, path string, content []byte) error {
	req := writeFileRequest{Path: path, Content: base64.StdEncoding.EncodeToString(content)}
	return p.bridgeJSON(ctx, instanceID, http.MethodPost, "/elasticclaw/v1/write-file", req, nil)
}

func (p *Provider) Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error) {
	vm, err := p.get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	return &types.ConnectInfo{
		Web: endpointURL(vm.Endpoint, "/"),
		Shell: &types.ShellConnect{
			Command: "aws",
			Args: append(p.awsBaseArgs(),
				"lambda-microvms", "create-microvm-auth-token",
				"--microvm-identifier", instanceID,
				"--expiration-in-minutes", strconv.Itoa(p.cfg.TokenExpirationMinutes),
				"--allowed-ports", fmt.Sprintf(`[{"port":%d}]`, p.cfg.BridgePort),
			),
		},
	}, nil
}

func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	_, err := p.aws(ctx, "lambda-microvms", "suspend-microvm", "--microvm-identifier", instanceID)
	return err
}

func (p *Provider) Start(ctx context.Context, instanceID string) error {
	_, err := p.aws(ctx, "lambda-microvms", "resume-microvm", "--microvm-identifier", instanceID)
	return err
}

func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	if keepState {
		return p.Stop(ctx, instanceID)
	}
	_, err := p.aws(ctx, "lambda-microvms", "terminate-microvm", "--microvm-identifier", instanceID)
	return err
}

func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	out, err := p.aws(ctx, "lambda-microvms", "list-microvms", "--output", "json")
	if err != nil {
		return nil, err
	}
	return parseListMicroVMs(out)
}

func parseListMicroVMs(data []byte) ([]*types.Instance, error) {
	var resp struct {
		Items []getMicroVMResponse `json:"items"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse list microvms response: %w", err)
	}
	instances := make([]*types.Instance, 0, len(resp.Items))
	for _, vm := range resp.Items {
		instances = append(instances, &types.Instance{
			ID:       vm.MicroVMID,
			Name:     vm.MicroVMID,
			Provider: "lambda-microvms",
			Status:   mapState(vm.State),
			ProviderMeta: map[string]string{
				"endpoint": vm.Endpoint,
			},
		})
	}
	return instances, nil
}

func buildInitializationPayload(req types.CreateRequest) (string, error) {
	files := make(map[string]string, len(req.TemplateFiles))
	for path, content := range req.TemplateFiles {
		files[path] = base64.StdEncoding.EncodeToString(content)
	}
	payload := runHookPayload{
		Version:       1,
		Name:          req.Name,
		Env:           req.Env,
		TemplateFiles: files,
		StateMount:    req.StateMount,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if len(data) > 16*1024*1024 {
		return "", fmt.Errorf("lambda microvms initialization payload exceeds 16MiB")
	}
	return string(data), nil
}

func writeRunHookPayloadFile(payload string) (string, func(), error) {
	if payload == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "elasticclaw-lambda-microvm-run-hook-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("create run hook payload file: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("secure run hook payload file: %w", err)
	}
	if _, err := f.WriteString(payload); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write run hook payload file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close run hook payload file: %w", err)
	}
	return "file://" + path, cleanup, nil
}

func (p *Provider) runMicroVMArgs(runHookPayloadArg string) ([]string, error) {
	args := []string{"lambda-microvms", "run-microvm", "--image-identifier", p.cfg.ImageIdentifier}
	if p.cfg.ImageVersion != "" {
		args = append(args, "--image-version", p.cfg.ImageVersion)
	}
	if p.cfg.ExecutionRoleARN != "" {
		args = append(args, "--execution-role-arn", p.cfg.ExecutionRoleARN)
	}
	if len(p.cfg.IngressNetworkConnectors) > 0 {
		args = append(args, "--ingress-network-connectors")
		args = append(args, p.cfg.IngressNetworkConnectors...)
	}
	if len(p.cfg.EgressNetworkConnectors) > 0 {
		args = append(args, "--egress-network-connectors")
		args = append(args, p.cfg.EgressNetworkConnectors...)
	}
	if p.cfg.IdleMaxDurationSeconds > 0 || p.cfg.SuspendedDurationSeconds > 0 || p.cfg.AutoResume != nil {
		idle := map[string]interface{}{
			"maxIdleDurationSeconds":   p.cfg.IdleMaxDurationSeconds,
			"suspendedDurationSeconds": p.cfg.SuspendedDurationSeconds,
			"autoResumeEnabled":        *p.cfg.AutoResume,
		}
		data, err := json.Marshal(idle)
		if err != nil {
			return nil, err
		}
		args = append(args, "--idle-policy", string(data))
	}
	if p.cfg.MaximumDurationSeconds > 0 {
		args = append(args, "--maximum-duration-in-seconds", strconv.Itoa(p.cfg.MaximumDurationSeconds))
	}
	if runHookPayloadArg != "" {
		args = append(args, "--run-hook-payload", runHookPayloadArg)
	}
	args = append(args, "--output", "json")
	return args, nil
}

func (p *Provider) get(ctx context.Context, instanceID string) (*getMicroVMResponse, error) {
	out, err := p.aws(ctx, "lambda-microvms", "get-microvm", "--microvm-identifier", instanceID, "--output", "json")
	if err != nil {
		return nil, err
	}
	var resp getMicroVMResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse get microvm response: %w", err)
	}
	return &resp, nil
}

func (p *Provider) authToken(ctx context.Context, instanceID string) (string, error) {
	allowedPorts := fmt.Sprintf(`[{"port":%d}]`, p.cfg.BridgePort)
	out, err := p.aws(ctx,
		"lambda-microvms", "create-microvm-auth-token",
		"--microvm-identifier", instanceID,
		"--expiration-in-minutes", strconv.Itoa(p.cfg.TokenExpirationMinutes),
		"--allowed-ports", allowedPorts,
		"--output", "json",
	)
	if err != nil {
		return "", err
	}
	var resp authTokenResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("parse microvm auth token response: %w", err)
	}
	if token := resp.AuthToken["X-aws-proxy-auth"]; token != "" {
		return token, nil
	}
	return "", fmt.Errorf("microvm auth token response missing X-aws-proxy-auth")
}

func (p *Provider) bridgeJSON(ctx context.Context, instanceID, method, path string, body interface{}, out interface{}) error {
	vm, err := p.get(ctx, instanceID)
	if err != nil {
		return err
	}
	return p.bridgeJSONAtEndpoint(ctx, instanceID, vm.Endpoint, method, path, body, out)
}

func (p *Provider) initialize(ctx context.Context, instanceID, endpoint, payload string) error {
	initCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var lastErr error
	for {
		if strings.TrimSpace(endpoint) == "" {
			vm, getErr := p.get(initCtx, instanceID)
			if getErr != nil {
				lastErr = getErr
			} else {
				endpoint = vm.Endpoint
			}
		}
		if strings.TrimSpace(endpoint) != "" {
			lastErr = p.bridgeJSONAtEndpoint(initCtx, instanceID, endpoint, http.MethodPost, "/elasticclaw/v1/init", json.RawMessage(payload), nil)
			if lastErr == nil {
				return nil
			}
		}
		select {
		case <-initCtx.Done():
			return fmt.Errorf("bridge did not become ready: %w", lastErr)
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *Provider) bridgeJSONAtEndpoint(ctx context.Context, instanceID, endpoint, method, path string, body interface{}, out interface{}) error {
	token, err := p.authToken(ctx, instanceID)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpointURL(endpoint, path), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("X-aws-proxy-auth", token)
	req.Header.Set("X-aws-proxy-port", strconv.Itoa(p.cfg.BridgePort))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := strings.TrimSpace(string(bodySnippet))
		if detail == "" {
			return fmt.Errorf("microvm bridge %s %s returned HTTP %d", method, path, resp.StatusCode)
		}
		return fmt.Errorf("microvm bridge %s %s returned HTTP %d: %s", method, path, resp.StatusCode, detail)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	}
	return nil
}

func endpointURL(endpoint, requestPath string) string {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		base = "https://" + base
	}
	return base + "/" + strings.TrimLeft(requestPath, "/")
}

func (p *Provider) aws(ctx context.Context, args ...string) ([]byte, error) {
	fullArgs := append(p.awsBaseArgs(), args...)
	cmd := exec.CommandContext(ctx, "aws", fullArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("aws %s: %w: %s", strings.Join(fullArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (p *Provider) awsBaseArgs() []string {
	var args []string
	if p.cfg.Region != "" {
		args = append(args, "--region", p.cfg.Region)
	}
	if p.cfg.Profile != "" {
		args = append(args, "--profile", p.cfg.Profile)
	}
	return args
}

func mapState(state string) types.InstanceStatus {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "RUNNING", "ACTIVE":
		return types.StatusRunning
	case "PENDING", "CREATING", "STARTING":
		return types.StatusStarting
	case "SUSPENDED", "STOPPED":
		return types.StatusStopped
	case "TERMINATED", "DELETED":
		return types.StatusNotFound
	case "FAILED", "ERROR":
		return types.StatusError
	default:
		return types.StatusUnknown
	}
}
