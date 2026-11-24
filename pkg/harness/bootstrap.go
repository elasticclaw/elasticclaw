package harness

import (
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// OpenClaw is the name of the OpenClaw harness.
const OpenClaw = "openclaw"

// BootstrapInput is what the hub knows when it prepares a sandbox for a harness.
type BootstrapInput struct {
	LLMKeys      []*types.LLMKeyConfig
	SelectedKey  string
	DefaultModel string
}

// BootstrapConfig is the harness-specific setup the hub hands to the sandbox.
// The fields are opaque scripts and flags; only the harness knows their meaning.
type BootstrapConfig struct {
	ProviderConfig string // ELASTICCLAW_PROVIDER_CONFIG
	APIKeyAuthSync string // ELASTICCLAW_API_KEY_AUTH_SYNC
	OAuthAuthSync  string // ELASTICCLAW_OAUTH_AUTH_SYNC
	OnboardFlags   string // ELASTICCLAW_ONBOARD_FLAGS
}

// Bootstrap prepares a sandbox for one harness.
type Bootstrap interface {
	Name() string
	Config(in BootstrapInput) BootstrapConfig
}

var bootstrapRegistry = struct {
	mu         sync.RWMutex
	bootstraps map[string]Bootstrap
}{bootstraps: make(map[string]Bootstrap)}

// RegisterBootstrap adds a harness bootstrap, panicking on a duplicate name.
func RegisterBootstrap(b Bootstrap) {
	name := b.Name()
	bootstrapRegistry.mu.Lock()
	defer bootstrapRegistry.mu.Unlock()
	if _, ok := bootstrapRegistry.bootstraps[name]; ok {
		panic("harness bootstrap already registered: " + name)
	}
	bootstrapRegistry.bootstraps[name] = b
}

// LookupBootstrap returns the registered bootstrap for name.
func LookupBootstrap(name string) (Bootstrap, bool) {
	bootstrapRegistry.mu.RLock()
	defer bootstrapRegistry.mu.RUnlock()
	b, ok := bootstrapRegistry.bootstraps[name]
	return b, ok
}
