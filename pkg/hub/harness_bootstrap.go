package hub

import "github.com/elasticclaw/elasticclaw/pkg/harness"

// openClawBootstrap returns the OpenClaw harness bootstrap, registered by
// importing pkg/harness/openclaw. The hub always uses OpenClaw for now.
func openClawBootstrap() harness.Bootstrap {
	b, ok := harness.LookupBootstrap(harness.OpenClaw)
	if !ok {
		panic("OpenClaw bootstrap is not registered")
	}
	return b
}
