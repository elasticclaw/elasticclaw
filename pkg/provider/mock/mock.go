// Package mock provides an in-memory implementation of the provider.Provider
// interface for unit tests. It requires no external services (Daytona, Docker,
// AWS, ...): instance state lives in a map, states are programmable via
// SetStatus, and failures can be injected per operation with Fail/FailOnce.
package mock

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/provider"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Compile-time check that Provider satisfies the provider.Provider interface.
var _ provider.Provider = (*Provider)(nil)

// Op identifies a Provider operation for failure injection and call recording.
type Op string

const (
	OpCreate  Op = "create"
	OpStatus  Op = "status"
	OpExec    Op = "exec"
	OpConnect Op = "connect"
	OpStop    Op = "stop"
	OpStart   Op = "start"
	OpDestroy Op = "destroy"
	OpList    Op = "list"
)

// Call records a single invocation of a Provider method.
type Call struct {
	Op         Op
	InstanceID string
}

// Provider is an in-memory, thread-safe Provider implementation.
type Provider struct {
	mu sync.Mutex

	info       types.ProviderInfo
	instances  map[string]*types.Instance
	nextID     int
	execResult types.ExecResult
	connect    types.ConnectInfo

	failures     map[Op]error // persistent injected failures
	failuresOnce map[Op]error // consumed on first matching call

	calls []Call
}

// New returns a mock Provider with default metadata.
func New() *Provider {
	return &Provider{
		info: types.ProviderInfo{
			Name:         "mock",
			Type:         types.ProviderTypeStateful,
			Capabilities: []string{"exec", "stop", "start"},
		},
		instances:    make(map[string]*types.Instance),
		execResult:   types.ExecResult{ExitCode: 0},
		connect:      types.ConnectInfo{Shell: &types.ShellConnect{Command: "true"}},
		failures:     make(map[Op]error),
		failuresOnce: make(map[Op]error),
	}
}

// SetInfo overrides the provider metadata returned by Info.
func (p *Provider) SetInfo(info types.ProviderInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.info = info
}

// Fail injects a persistent failure for op. Pass err == nil to clear it.
func (p *Provider) Fail(op Op, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.failures, op)
		return
	}
	p.failures[op] = err
}

// FailOnce injects a failure that is consumed by the next call to op.
func (p *Provider) FailOnce(op Op, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failuresOnce[op] = err
}

// SetStatus programs the state of an existing instance. It creates the
// instance record if it does not exist yet, so tests can seed arbitrary states.
func (p *Provider) SetStatus(instanceID string, status types.InstanceStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	inst, ok := p.instances[instanceID]
	if !ok {
		inst = &types.Instance{
			ID:        instanceID,
			Name:      instanceID,
			Provider:  p.info.Name,
			CreatedAt: time.Now().UTC(),
		}
		p.instances[instanceID] = inst
	}
	inst.Status = status
}

// SetExecResult programs the result returned by Exec.
func (p *Provider) SetExecResult(res types.ExecResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execResult = res
}

// SetConnectInfo programs the result returned by Connect.
func (p *Provider) SetConnectInfo(info types.ConnectInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connect = info
}

// Calls returns a copy of all recorded Provider method invocations.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Call, len(p.calls))
	copy(out, p.calls)
	return out
}

// checkLocked records the call and returns any injected failure or context error.
// Callers must hold p.mu.
func (p *Provider) checkLocked(ctx context.Context, op Op, instanceID string) error {
	p.calls = append(p.calls, Call{Op: op, InstanceID: instanceID})
	if err := ctx.Err(); err != nil {
		return err
	}
	if err, ok := p.failuresOnce[op]; ok {
		delete(p.failuresOnce, op)
		return err
	}
	if err, ok := p.failures[op]; ok {
		return err
	}
	return nil
}

// Info returns provider metadata.
func (p *Provider) Info() types.ProviderInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

// Create provisions a new in-memory instance in the running state.
func (p *Provider) Create(ctx context.Context, req types.CreateRequest) (*types.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpCreate, req.Name); err != nil {
		return nil, err
	}
	p.nextID++
	inst := &types.Instance{
		ID:        fmt.Sprintf("mock-%d", p.nextID),
		Name:      req.Name,
		Provider:  p.info.Name,
		Status:    types.StatusRunning,
		CreatedAt: time.Now().UTC(),
	}
	p.instances[inst.ID] = inst
	cp := *inst
	return &cp, nil
}

// Status returns the current (possibly programmed) state of an instance.
func (p *Provider) Status(ctx context.Context, instanceID string) (types.InstanceStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpStatus, instanceID); err != nil {
		return types.StatusUnknown, err
	}
	inst, ok := p.instances[instanceID]
	if !ok {
		return types.StatusNotFound, nil
	}
	return inst.Status, nil
}

// Exec returns the programmed exec result.
func (p *Provider) Exec(ctx context.Context, instanceID string, cmd []string) (*types.ExecResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpExec, instanceID); err != nil {
		return nil, err
	}
	if _, ok := p.instances[instanceID]; !ok {
		return nil, fmt.Errorf("mock: instance %q not found", instanceID)
	}
	res := p.execResult
	return &res, nil
}

// Connect returns the programmed connection info.
func (p *Provider) Connect(ctx context.Context, instanceID string) (*types.ConnectInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpConnect, instanceID); err != nil {
		return nil, err
	}
	if _, ok := p.instances[instanceID]; !ok {
		return nil, fmt.Errorf("mock: instance %q not found", instanceID)
	}
	info := p.connect
	return &info, nil
}

// Stop transitions an instance to the stopped state.
func (p *Provider) Stop(ctx context.Context, instanceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpStop, instanceID); err != nil {
		return err
	}
	inst, ok := p.instances[instanceID]
	if !ok {
		return fmt.Errorf("mock: instance %q not found", instanceID)
	}
	inst.Status = types.StatusStopped
	return nil
}

// Start transitions a stopped instance back to running.
func (p *Provider) Start(ctx context.Context, instanceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpStart, instanceID); err != nil {
		return err
	}
	inst, ok := p.instances[instanceID]
	if !ok {
		return fmt.Errorf("mock: instance %q not found", instanceID)
	}
	inst.Status = types.StatusRunning
	return nil
}

// Destroy removes the instance from the in-memory map.
func (p *Provider) Destroy(ctx context.Context, instanceID string, keepState bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpDestroy, instanceID); err != nil {
		return err
	}
	if _, ok := p.instances[instanceID]; !ok {
		return fmt.Errorf("mock: instance %q not found", instanceID)
	}
	delete(p.instances, instanceID)
	return nil
}

// List returns copies of all instances, sorted by ID for determinism.
func (p *Provider) List(ctx context.Context) ([]*types.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, OpList, ""); err != nil {
		return nil, err
	}
	out := make([]*types.Instance, 0, len(p.instances))
	for _, inst := range p.instances {
		cp := *inst
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
