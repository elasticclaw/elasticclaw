package v2

import "strings"

// ConnectionCapability names operational capabilities a connection may expose.
// Workspace YAML may only restrict (set false); it cannot invent or enable
// capabilities beyond the provider capability model.
type ConnectionCapability string

const (
	CapObserveRuns       ConnectionCapability = "observe_runs"
	CapObserveChecks     ConnectionCapability = "observe_checks"
	CapTriggerRun        ConnectionCapability = "trigger_run"
	CapRetryRun          ConnectionCapability = "retry_run"
	CapCancelRun         ConnectionCapability = "cancel_run"
	CapFetchLogs         ConnectionCapability = "fetch_logs"
	CapReconcile         ConnectionCapability = "reconcile"
	CapExecuteCommand    ConnectionCapability = "execute_command"
	CapDependencyUpdate  ConnectionCapability = "dependency_update"
)

// AllConnectionCapabilities is the full set used by CI-style connections.
var AllConnectionCapabilities = []ConnectionCapability{
	CapObserveRuns,
	CapObserveChecks,
	CapTriggerRun,
	CapRetryRun,
	CapCancelRun,
	CapFetchLogs,
	CapReconcile,
	CapExecuteCommand,
	CapDependencyUpdate,
}

// Effect operations that require a resolved connection capability.
const (
	EffectCITrigger        = "ci.trigger"
	EffectCIRetry          = "ci.retry"
	EffectCICancel         = "ci.cancel"
	EffectExecRun          = "exec.run"
	EffectDependencyUpdate = "dependency.update"
)

// ProviderCapabilities returns the default capability set for a provider type.
// Unknown providers get no capabilities (effects that need them will fail validation).
func ProviderCapabilities(provider string) map[ConnectionCapability]bool {
	out := map[ConnectionCapability]bool{}
	switch provider {
	case "github_actions", "depot", "jenkins":
		for _, c := range AllConnectionCapabilities {
			out[c] = true
		}
	case "github":
		// Source-control / review providers: observe-oriented defaults only.
		out[CapObserveRuns] = true
		out[CapObserveChecks] = true
		out[CapFetchLogs] = true
		out[CapReconcile] = true
	case "linear", "greptile":
		out[CapObserveRuns] = true
		out[CapReconcile] = true
	default:
		if isExecutionProvider(provider) {
			// Execution providers grant deterministic command and dependency-update
			// effects. The workspace may narrow these via capability_restrictions.
			out[CapExecuteCommand] = true
			out[CapDependencyUpdate] = true
		}
	}
	return out
}

func isExecutionProvider(provider string) bool {
	for _, prefix := range []string{"daytona", "replicated", "exedev", "docker", "lambda-microvms"} {
		if strings.HasPrefix(provider, prefix) {
			return true
		}
	}
	return false
}


// ResolveCapabilities intersects provider defaults with workspace restrictions.
// Restrictions may only set a capability to false; setting true when the
// provider lacks it is rejected by validation before this is used.
func ResolveCapabilities(provider string, restrictions map[string]bool) map[ConnectionCapability]bool {
	base := ProviderCapabilities(provider)
	resolved := make(map[ConnectionCapability]bool, len(base))
	for c, allowed := range base {
		resolved[c] = allowed
	}
	for name, enabled := range restrictions {
		cap := ConnectionCapability(name)
		if !enabled {
			// Narrow: always allowed to set false.
			resolved[cap] = false
			continue
		}
		// Enabling is only valid if provider already supports it.
		if base[cap] {
			resolved[cap] = true
		}
	}
	return resolved
}

// CapabilityForEffect maps a structured effect operation to the capability it needs.
func CapabilityForEffect(op string) (ConnectionCapability, bool) {
	switch op {
	case EffectCITrigger:
		return CapTriggerRun, true
	case EffectCIRetry:
		return CapRetryRun, true
	case EffectCICancel:
		return CapCancelRun, true
	case EffectExecRun:
		return CapExecuteCommand, true
	case EffectDependencyUpdate:
		return CapDependencyUpdate, true
	default:
		return "", false
	}
}
