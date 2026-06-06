// Package replicated implements an ElasticClaw provider that uses
// Replicated Compatibility Matrix (CMX) VMs to run claw instances.
package replicated

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	DefaultAPIURL       = "https://api.replicated.com/vendor/v3"
	DefaultInstanceType = "r1.large"
	DefaultTTL          = "48h"
	DefaultDistribution = "ubuntu"
	DefaultVersion      = "24.04"
	DefaultDiskGiB      = 50
)

// SSHUserFromPublicKey extracts the username from an SSH public key comment.
// Replicated CMX uses the key comment (e.g. "elasticclaw@hub") as the Linux username,
// taking the part before the @ sign.
// "ssh-ed25519 AAAA... elasticclaw@hub" → "elasticclaw"
func SSHUserFromPublicKey(authorizedKey string) string {
	parts := strings.Fields(strings.TrimSpace(authorizedKey))
	if len(parts) >= 3 {
		comment := parts[2]
		if atIdx := strings.Index(comment, "@"); atIdx > 0 {
			return comment[:atIdx]
		}
		return comment
	}
	return "ubuntu" // fallback
}

// Provider implements the Replicated CMX VM provider.
type Provider struct {
	apiURL       string
	token        string
	defaultTTL   string
	defaultType  string
	sshPublicKey string   // hub's generated key, always injected
	extraKeys    []string // operator debug keys from hub config
	http         *http.Client
}

// Config holds provider configuration from hub.yaml.
type Config struct {
	APIURL      string `yaml:"api_url,omitempty"`
	Token       string `yaml:"token"`
	DefaultTTL  string `yaml:"default_ttl,omitempty"`
	DefaultType string `yaml:"default_instance_type,omitempty"`
	// HubPublicKey is set at runtime from the hub's generated identity — not from config file.
	HubPublicKey string `yaml:"-"`
	// ExtraPublicKeys are additional keys from hub config (e.g. operator's personal key for debug access).
	ExtraPublicKeys []string `yaml:"-"`
}

// New creates a Replicated CMX provider from config.
func New(cfg Config) (*Provider, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("replicated provider requires a token")
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	ttl := cfg.DefaultTTL
	if ttl == "" {
		ttl = DefaultTTL
	}
	instanceType := cfg.DefaultType
	if instanceType == "" {
		instanceType = DefaultInstanceType
	}
	return &Provider{
		apiURL:       strings.TrimRight(apiURL, "/"),
		token:        cfg.Token,
		defaultTTL:   ttl,
		defaultType:  instanceType,
		sshPublicKey: cfg.HubPublicKey,
		extraKeys:    cfg.ExtraPublicKeys,
		http:         &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// ─── API types ────────────────────────────────────────────────────────────────

type createVMRequest struct {
	Count        int64    `json:"count"`
	DiskGiB      int64    `json:"disk_gib"`
	Distribution string   `json:"distribution"`
	InstanceType string   `json:"instance_type"`
	Name         string   `json:"name"`
	PublicKeys   []string `json:"public_keys,omitempty"`
	TTL          string   `json:"ttl"`
	Version      string   `json:"version"`
}

type vmResponse struct {
	VMs []vmDetail `json:"vms"`
}

type vmDetail struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Status            string `json:"status"` // assigned, provisioning, preparing, running, terminated
	Distribution      string `json:"distribution"`
	InstanceType      string `json:"instance_type"`
	NetworkID         string `json:"network_id"`
	DirectSSHEndpoint string `json:"direct_ssh_endpoint"`
	DirectSSHPort     int    `json:"direct_ssh_port"`
}

// ─── Provider methods ─────────────────────────────────────────────────────────

// CreateVM provisions a Replicated CMX VM and returns the VM ID.
// It injects the hub's SSH public key so the hub can bootstrap the claw.
func (p *Provider) CreateVM(ctx context.Context, req VMCreateRequest) (string, error) {
	instanceType := req.InstanceType
	if instanceType == "" {
		instanceType = p.defaultType
	}
	ttl := req.TTL
	if ttl == "" {
		ttl = p.defaultTTL
	}

	body := createVMRequest{
		Count:        1,
		DiskGiB:      DefaultDiskGiB,
		Distribution: DefaultDistribution,
		InstanceType: instanceType,
		Name:         req.Name,
		TTL:          ttl,
		Version:      DefaultVersion,
	}
	// Always include the hub's generated key (base64-encoded full authorized_keys line)
	if p.sshPublicKey != "" {
		encoded := encodeSSHKey(p.sshPublicKey)
		log.Printf("SSH key being sent to Replicated API (base64 of: %s)", strings.TrimSpace(p.sshPublicKey))
		body.PublicKeys = append(body.PublicKeys, encoded)
	}
	// Append operator debug keys
	for _, raw := range p.extraKeys {
		if raw = strings.TrimSpace(raw); raw != "" {
			body.PublicKeys = append(body.PublicKeys, encodeSSHKey(raw))
		}
	}

	data, err := p.post(ctx, "/vm", body)
	if err != nil {
		return "", fmt.Errorf("create VM: %w", err)
	}

	var resp vmResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse create VM response: %w", err)
	}
	if len(resp.VMs) == 0 {
		return "", fmt.Errorf("no VM returned in response")
	}
	return resp.VMs[0].ID, nil
}

// WaitForVM polls until the VM is running or ctx is cancelled.
// Returns the VM ID (hostname is <vmid>@replicatedvm.com).
func (p *Provider) WaitForVM(ctx context.Context, vmID string) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			vm, err := p.GetVM(ctx, vmID)
			if err != nil {
				return fmt.Errorf("poll VM: %w", err)
			}
			switch vm.Status {
			case "running":
				return nil
			case "terminated", "error":
				return fmt.Errorf("VM %s entered terminal state: %s", vmID, vm.Status)
			}
			// assigned, starting, etc — keep polling
		}
	}
}

// GetVM fetches the current status of a VM.
func (p *Provider) GetVM(ctx context.Context, vmID string) (*vmDetail, error) {
	data, err := p.get(ctx, "/vm/"+vmID)
	if err != nil {
		return nil, err
	}
	// Response is {"vm": {...}}
	var wrapped struct {
		VM *vmDetail `json:"vm"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parse VM response: %w", err)
	}
	if wrapped.VM == nil {
		return nil, fmt.Errorf("no VM in response")
	}
	return wrapped.VM, nil
}

// DeleteVM terminates a CMX VM.
func (p *Provider) DeleteVM(ctx context.Context, vmID string) error {
	_, err := p.delete(ctx, "/vm/"+vmID)
	return err
}

// VMHostname returns the SSH hostname for a CMX VM.
// VMs are accessed as <vmid>@replicatedvm.com.
func VMHostname(vmID string) string {
	return vmID + "@replicatedvm.com"
}

// ─── Satisfy types.CreateRequest interface for hub provisioning ───────────────

// VMCreateRequest is the input for creating a CMX VM via the hub.
type VMCreateRequest struct {
	Name         string
	InstanceType string // e.g. r1.small, r1.large (overrides provider default)
	TTL          string // e.g. 2h, 24h (overrides provider default, max 48h)
}

// ProvisionClaw creates a VM and returns the VM ID. The hub's poller monitors
// VM status and triggers bootstrapReplicated separately once the VM is running.
//
// Neither templateFiles nor env can be injected at creation time because the
// Replicated CMX API only provisions the VM — SSH is not available until the VM
// is running. The hub's bootstrapReplicated path handles both file and env
// injection separately after the VM is running via SSH.
func (p *Provider) ProvisionClaw(
	ctx context.Context,
	req VMCreateRequest,
	templateFiles map[string][]byte,
	env map[string]string,
) (string, error) {
	if len(templateFiles) > 0 {
		return "", fmt.Errorf("replicated provider does not support templateFiles in ProvisionClaw: files must be injected after VM is running via SSH")
	}
	if len(env) > 0 {
		return "", fmt.Errorf("replicated provider does not support env in ProvisionClaw: environment variables must be set after VM is running via SSH")
	}
	vmID, err := p.CreateVM(ctx, req)
	if err != nil {
		return "", err
	}
	// Return immediately — the hub's poller monitors VM status and triggers bootstrap.
	log.Printf("Replicated VM created: %s", vmID)
	return vmID, nil
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (p *Provider) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return p.do(req)
}

func (p *Provider) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	return p.do(req)
}

func (p *Provider) delete(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	return p.do(req)
}

func (p *Provider) do(req *http.Request) ([]byte, error) {
	req.Header.Set("Authorization", p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s %s: read response body: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, req.URL.Path, string(data))
	}
	return data, nil
}

// ─── types.Provider adapter ───────────────────────────────────────────────────

// Create implements the types.Provider interface used by the existing cmd layer.
// For CMX VMs we create the VM and return a placeholder instance.
//
// Note: Replicated CMX VMs cannot inject TemplateFiles or Env at creation time
// because the VM must be running before SSH is available. The hub's
// bootstrapReplicated path handles file injection separately after the VM is
// running. If TemplateFiles or Env are provided here, an error is returned
// to avoid silent data loss.
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	if len(req.TemplateFiles) > 0 {
		return nil, fmt.Errorf("replicated provider does not support TemplateFiles in Create: files must be injected after VM is running via SSH")
	}
	if len(req.Env) > 0 {
		return nil, fmt.Errorf("replicated provider does not support Env in Create: environment variables must be set after VM is running via SSH")
	}
	vmID, err := p.CreateVM(ctx, VMCreateRequest{Name: req.Name})
	if err != nil {
		return nil, err
	}
	return &types.Instance{
		ID:       vmID,
		Name:     req.Name,
		Provider: "replicated",
		Status:   types.StatusStarting,
		ProviderMeta: map[string]string{
			"vm_id":    vmID,
			"hostname": VMHostname(vmID),
		},
	}, nil
}

// Destroy implements types.Provider — deletes the CMX VM.
func (p *Provider) Destroy(ctx context.Context, instanceID string, _ bool) error {
	return p.DeleteVM(ctx, instanceID)
}

// encodeSSHKey base64-encodes the full authorized_keys line for the Replicated API.
func encodeSSHKey(authorizedKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(authorizedKey)))
}
