package types

import "testing"

// The prefixes are a WIRE FORMAT, not a label: bridges already running in the
// field emit these exact strings, and a claw only gets a newer bridge when it
// is created after a release. Changing a byte here silently un-recognises
// every live claw and brings back the NEXT-725 loop, so the literals are
// pinned to what the deployed bridges write.
func TestBridgeErrorPrefixesMatchDeployedBridges(t *testing.T) {
	if BridgeErrorPrefix != "⚠️ claw-bridge error:" {
		t.Fatalf("live-turn prefix changed: %q", BridgeErrorPrefix)
	}
	if BridgeReplayErrorPrefix != "⚠️ error:" {
		t.Fatalf("replay prefix changed: %q", BridgeReplayErrorPrefix)
	}
}
