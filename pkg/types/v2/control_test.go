package v2_test

import (
	"strings"
	"testing"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func stateVersion(version uint64) *uint64 { return &version }

func TestControlEnvelopeRequiresTypedDirectionAndStateVersion(t *testing.T) {
	valid := v2.ControlEnvelope{
		ProtocolVersion:      v2.ControlProtocolVersion,
		MessageID:            "message-1",
		Kind:                 v2.MessageAgentTaskCompleted,
		RunID:                "run-1",
		AttemptID:            "attempt-1",
		TaskID:               "task-1",
		ExpectedStateVersion: stateVersion(7),
	}
	if err := v2.ValidateControlEnvelope(valid, v2.DirectionClawToHub); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}

	wrongDirection := valid
	if err := v2.ValidateControlEnvelope(wrongDirection, v2.DirectionHubToClaw); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("wrong direction error = %v", err)
	}

	staleUnsafe := valid
	staleUnsafe.ExpectedStateVersion = nil
	if err := v2.ValidateControlEnvelope(staleUnsafe, v2.DirectionClawToHub); err == nil || !strings.Contains(err.Error(), "expected_state_version") {
		t.Fatalf("missing state version error = %v", err)
	}
}

func TestControlCatalogContainsNoConversationMessages(t *testing.T) {
	kinds := []v2.ControlMessageKind{
		v2.MessageWorkflowSync,
		v2.MessageAgentTaskAssign,
		v2.MessageAgentTaskCompleted,
		v2.MessageDeliverySubmitted,
	}
	for _, kind := range kinds {
		if strings.Contains(string(kind), "message") || strings.Contains(string(kind), "conversation") {
			t.Fatalf("control kind %q crosses conversation boundary", kind)
		}
	}
}

func TestBridgeCompatibilityMatrix(t *testing.T) {
	oldBridge := v2.BridgeRegistration{Protocols: []string{v2.ProtocolConversationV1}}
	newBridge := v2.BridgeRegistration{Protocols: []string{v2.ProtocolConversationV1, v2.ProtocolControlV2}}

	if err := v2.ValidateBridgeForWorkflow("v1", oldBridge); err != nil {
		t.Fatalf("old bridge + v1: %v", err)
	}
	if err := v2.ValidateBridgeForWorkflow("v1", newBridge); err != nil {
		t.Fatalf("new bridge + v1: %v", err)
	}
	if err := v2.ValidateBridgeForWorkflow("2", newBridge); err != nil {
		t.Fatalf("new bridge + v2: %v", err)
	}
	if err := v2.ValidateBridgeForWorkflow("2", oldBridge); err == nil || !strings.Contains(err.Error(), v2.ProtocolControlV2) {
		t.Fatalf("old bridge + v2 error = %v", err)
	}
}
