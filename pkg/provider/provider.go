package provider

import (
	"context"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider defines the interface for execution backends
type Provider interface {
	// Info returns provider metadata
	Info() types.ProviderInfo

	// Create provisions a new instance
	Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error)

	// Status checks current instance state
	Status(ctx context.Context, instanceID string) (types.InstanceStatus, error)

	// Exec runs a command inside the instance
	Exec(ctx context.Context, instanceID string, cmd []string) (*types.ExecResult, error)

	// Connect returns connection info
	Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error)

	// Stop pauses/hibernates (stateful providers only)
	Stop(ctx context.Context, instanceID string) error

	// Start resumes a stopped instance
	Start(ctx context.Context, instanceID string) error

	// Destroy tears down the instance
	Destroy(ctx context.Context, instanceID string, keepState bool) error

	// List returns all instances managed by this provider
	List(ctx context.Context) ([]*types.Instance, error)
}

// Registry holds available providers
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates a new provider registry
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register adds a provider to the registry
func (r *Registry) Register(name string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
}

// Get returns a provider by name
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// List returns all registered provider names
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	return names
}

// ListWithInfo returns info for all registered providers
func (r *Registry) ListWithInfo() []types.ProviderInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	infos := make([]types.ProviderInfo, 0, len(r.providers))
	for _, p := range r.providers {
		infos = append(infos, p.Info())
	}
	return infos
}
