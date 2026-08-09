package v2

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	ControlProtocolVersion = 2
	ProtocolConversationV1 = "conversation.v1"
	ProtocolControlV2      = "control.v2"
)

// ControlMessageKind identifies a typed control-plane operation. Conversation
// messages are deliberately absent: freeform text is never a v2 control input.
type ControlMessageKind string

const (
	MessageWorkflowSync       ControlMessageKind = "workflow.sync"
	MessageAgentTaskAssign    ControlMessageKind = "agent.task.assign"
	MessageAgentTaskCancel    ControlMessageKind = "agent.task.cancel"
	MessageArtifactRequest    ControlMessageKind = "artifact.request"
	MessageCheckpointRequest  ControlMessageKind = "checkpoint.request"
	MessageRunSuspend         ControlMessageKind = "run.suspend"
	MessageRunResume          ControlMessageKind = "run.resume"
	MessageRunTerminate       ControlMessageKind = "run.terminate"
	MessageAgentTaskStarted   ControlMessageKind = "agent.task.started"
	MessageAgentTaskHeartbeat ControlMessageKind = "agent.task.heartbeat"
	MessageAgentTaskCompleted ControlMessageKind = "agent.task.completed"
	MessageAgentTaskFailed    ControlMessageKind = "agent.task.failed"
	MessagePlanSubmitted      ControlMessageKind = "plan.submitted"
	MessageDeliverySubmitted  ControlMessageKind = "delivery.submitted"
	MessagePullRequestClaimed ControlMessageKind = "pull_request.claimed"
	MessageHelpRequested      ControlMessageKind = "help.requested"
)

type ControlDirection string

const (
	DirectionHubToClaw ControlDirection = "hub_to_claw"
	DirectionClawToHub ControlDirection = "claw_to_hub"
)

// ControlEnvelope carries the durable identity required for authentication,
// deduplication, causality, and compare-and-swap state changes. Payload is
// decoded only after the kind and direction have been accepted.
type ControlEnvelope struct {
	ProtocolVersion      int                `json:"protocol_version"`
	MessageID            string             `json:"message_id"`
	Kind                 ControlMessageKind `json:"kind"`
	RunID                string             `json:"run_id"`
	AttemptID            string             `json:"attempt_id,omitempty"`
	TaskID               string             `json:"task_id,omitempty"`
	ExpectedStateVersion *uint64            `json:"expected_state_version,omitempty"`
	CausationID          string             `json:"causation_id,omitempty"`
	SentAt               time.Time          `json:"sent_at,omitempty"`
	Payload              json.RawMessage    `json:"payload,omitempty"`
}

// ControlDisposition is durably recorded for every received control message.
type ControlDisposition string

const (
	DispositionAccepted     ControlDisposition = "accepted"
	DispositionDuplicate    ControlDisposition = "duplicate"
	DispositionStaleState   ControlDisposition = "stale_state"
	DispositionRejected     ControlDisposition = "rejected"
	DispositionUnauthorized ControlDisposition = "unauthorized"
)

type ControlReceipt struct {
	MessageID    string             `json:"message_id"`
	Disposition  ControlDisposition `json:"disposition"`
	StateVersion uint64             `json:"state_version"`
	Reason       string             `json:"reason,omitempty"`
}

const (
	ControlFrameRegister   = "register"
	ControlFrameRegistered = "registered"
	ControlFrameEnvelope   = "envelope"
	ControlFrameReceipt    = "receipt"
)

// ControlRegistration authenticates the bridge's dedicated workflow channel.
// It is intentionally separate from the conversation WebSocket registration.
type ControlRegistration struct {
	Token     string             `json:"token"`
	ClawID    string             `json:"claw_id"`
	RunID     string             `json:"run_id"`
	AttemptID string             `json:"attempt_id"`
	Bridge    BridgeRegistration `json:"bridge"`
}

// ControlFrame is the only wire frame accepted on /claw/control/ws.
type ControlFrame struct {
	Type         string               `json:"type"`
	Registration *ControlRegistration `json:"registration,omitempty"`
	Snapshot     *WorkflowSnapshot    `json:"snapshot,omitempty"`
	Envelope     *ControlEnvelope     `json:"envelope,omitempty"`
	Receipt      *ControlReceipt      `json:"receipt,omitempty"`
	Error        string               `json:"error,omitempty"`
}

// WorkflowEvent is the durable input record consumed by the v2 engine. Its
// payload is typed by Kind; freeform conversation messages have no producer in
// this event model.
type WorkflowEvent struct {
	ID                   string                 `json:"id"`
	RunID                string                 `json:"run_id"`
	Kind                 string                 `json:"kind"`
	ExpectedStateVersion *uint64                `json:"expected_state_version,omitempty"`
	CausationID          string                 `json:"causation_id,omitempty"`
	Provenance           EvidenceProvenance     `json:"provenance"`
	Payload              map[string]interface{} `json:"payload,omitempty"`
	Disposition          ControlDisposition     `json:"disposition,omitempty"`
	ReceivedAt           time.Time              `json:"received_at"`
}

var hubToClawKinds = map[ControlMessageKind]bool{
	MessageWorkflowSync: true, MessageAgentTaskAssign: true,
	MessageAgentTaskCancel: true, MessageArtifactRequest: true,
	MessageCheckpointRequest: true, MessageRunSuspend: true,
	MessageRunResume: true, MessageRunTerminate: true,
}

var clawToHubKinds = map[ControlMessageKind]bool{
	MessageAgentTaskStarted: true, MessageAgentTaskHeartbeat: true,
	MessageAgentTaskCompleted: true, MessageAgentTaskFailed: true,
	MessagePlanSubmitted: true, MessageDeliverySubmitted: true,
	MessagePullRequestClaimed: true, MessageHelpRequested: true,
}

var stateChangingKinds = map[ControlMessageKind]bool{
	MessageRunSuspend: true, MessageRunResume: true, MessageRunTerminate: true,
	MessageAgentTaskStarted: true, MessageAgentTaskCompleted: true,
	MessageAgentTaskFailed: true, MessagePlanSubmitted: true,
	MessageDeliverySubmitted: true, MessagePullRequestClaimed: true,
}

var taskKinds = map[ControlMessageKind]bool{
	MessageAgentTaskAssign: true, MessageAgentTaskCancel: true,
	MessageAgentTaskStarted: true, MessageAgentTaskHeartbeat: true,
	MessageAgentTaskCompleted: true, MessageAgentTaskFailed: true,
}

// ValidateControlEnvelope rejects unknown protocol versions, missing durable
// identities, and messages sent in the wrong direction.
func ValidateControlEnvelope(envelope ControlEnvelope, direction ControlDirection) error {
	if envelope.ProtocolVersion != ControlProtocolVersion {
		return fmt.Errorf("unsupported control protocol version %d", envelope.ProtocolVersion)
	}
	if strings.TrimSpace(envelope.MessageID) == "" {
		return fmt.Errorf("message_id is required")
	}
	if strings.TrimSpace(envelope.RunID) == "" {
		return fmt.Errorf("run_id is required")
	}
	var allowed bool
	switch direction {
	case DirectionHubToClaw:
		allowed = hubToClawKinds[envelope.Kind]
	case DirectionClawToHub:
		allowed = clawToHubKinds[envelope.Kind]
	default:
		return fmt.Errorf("unsupported control direction %q", direction)
	}
	if !allowed {
		return fmt.Errorf("message kind %q is not allowed for direction %s", envelope.Kind, direction)
	}
	if direction == DirectionClawToHub && strings.TrimSpace(envelope.AttemptID) == "" {
		return fmt.Errorf("attempt_id is required for claw-to-hub messages")
	}
	if taskKinds[envelope.Kind] && strings.TrimSpace(envelope.TaskID) == "" {
		return fmt.Errorf("task_id is required for message kind %q", envelope.Kind)
	}
	if stateChangingKinds[envelope.Kind] && envelope.ExpectedStateVersion == nil {
		return fmt.Errorf("expected_state_version is required for state-changing message kind %q", envelope.Kind)
	}
	return nil
}

// BridgeRegistration advertises protocols separately from optional feature
// capabilities so a hub can reject a v2 run before provisioning an old bridge.
type BridgeRegistration struct {
	BridgeVersion string   `json:"bridge_version"`
	Protocols     []string `json:"protocols"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

func (r BridgeRegistration) SupportsProtocol(protocol string) bool {
	for _, candidate := range r.Protocols {
		if candidate == protocol {
			return true
		}
	}
	return false
}

// ValidateBridgeForWorkflow enforces the rolling-upgrade compatibility
// matrix. V1 remains compatible with old and new bridges; v2 requires the
// dedicated typed control protocol.
func ValidateBridgeForWorkflow(schemaVersion string, registration BridgeRegistration) error {
	if !IsV2(schemaVersion) {
		return nil
	}
	if !registration.SupportsProtocol(ProtocolControlV2) {
		return fmt.Errorf("workflow schema v2 requires bridge protocol %s", ProtocolControlV2)
	}
	return nil
}

type AgentTaskStatus string

const (
	AgentTaskAssigned  AgentTaskStatus = "assigned"
	AgentTaskRunning   AgentTaskStatus = "running"
	AgentTaskCompleted AgentTaskStatus = "completed"
	AgentTaskFailed    AgentTaskStatus = "failed"
	AgentTaskCancelled AgentTaskStatus = "cancelled"
	AgentTaskTimedOut  AgentTaskStatus = "timed_out"
)

// AgentTask is a durable long-running effect attached to a run attempt.
type AgentTask struct {
	ID                string          `json:"id"`
	RunID             string          `json:"run_id"`
	AttemptID         string          `json:"attempt_id"`
	State             string          `json:"state"`
	StateVersion      uint64          `json:"state_version"`
	Status            AgentTaskStatus `json:"status"`
	Instructions      string          `json:"instructions"`
	AllowedActions    []string        `json:"allowed_actions,omitempty"`
	RequiredArtifacts []string        `json:"required_artifacts,omitempty"`
	HeartbeatDeadline time.Time       `json:"heartbeat_deadline"`
	Deadline          time.Time       `json:"deadline"`
}

func (s AgentTaskStatus) Terminal() bool {
	switch s {
	case AgentTaskCompleted, AgentTaskFailed, AgentTaskCancelled, AgentTaskTimedOut:
		return true
	default:
		return false
	}
}

// WorkflowSnapshot is sent on registration/reconnect. The hub owns these
// values; a claw may request actions but cannot assign its own workflow state.
type WorkflowSnapshot struct {
	RunID             string                `json:"run_id"`
	AttemptID         string                `json:"attempt_id,omitempty"`
	State             string                `json:"state"`
	DisplayPhase      DisplayPhase          `json:"display_phase"`
	StateVersion      uint64                `json:"state_version"`
	CurrentTask       *AgentTask            `json:"current_task,omitempty"`
	AllowedActions    []string              `json:"allowed_actions,omitempty"`
	RequiredArtifacts []string              `json:"required_artifacts,omitempty"`
	ContextBundle     *ContextBundleRef     `json:"context_bundle,omitempty"`
	Delivery          []VerifiedPullRequest `json:"delivery,omitempty"`
}

// EvidenceProvenance identifies who observed a fact and through which trusted
// adapter. Agent observations can be recorded but cannot populate protected
// fact namespaces.
type EvidenceProvenance struct {
	Producer   string    `json:"producer"`
	Connection string    `json:"connection,omitempty"`
	Principal  string    `json:"principal,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	ExternalID string    `json:"external_id,omitempty"`
	Reconciled bool      `json:"reconciled,omitempty"`
}

type ContextBundleRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// ContextBundle records the exact source revisions/results supplied to a run.
type ContextBundle struct {
	ID        string                `json:"id"`
	RunID     string                `json:"run_id"`
	Revision  string                `json:"revision"`
	CreatedAt time.Time             `json:"created_at"`
	Sources   []ContextBundleSource `json:"sources"`
}

type ContextBundleSource struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Scope          string   `json:"scope"`
	Required       bool     `json:"required"`
	Repositories   []string `json:"repositories,omitempty"`
	Status         string   `json:"status"`
	SourceRevision string   `json:"source_revision,omitempty"`
	ContentDigest  string   `json:"content_digest,omitempty"`
	Documents      []string `json:"documents,omitempty"`
	Error          string   `json:"error,omitempty"`
}

// DeliveryManifest is the untrusted claw submission. URLs are claims until the
// hub resolves and verifies them through workspace source-control connections.
type DeliveryManifest struct {
	PullRequests []PullRequestClaim `json:"pull_requests,omitempty"`
}

type PullRequestClaim struct {
	URL string `json:"url"`
	// Supersedes is reserved for a hub-owned source-control reconciler. Claw
	// submissions that set it are rejected rather than trusted to remove PRs.
	Supersedes string `json:"supersedes,omitempty"`
}

// VerifiedPullRequest is the authoritative, append-only delivery subject
// derived by the hub. Evidence is always tied to HeadSHA.
type VerifiedPullRequest struct {
	ID             string             `json:"id"`
	URL            string             `json:"url"`
	Repository     string             `json:"repository"`
	RepositoryName string             `json:"repository_name"`
	Number         int                `json:"number"`
	SourceBranch   string             `json:"source_branch"`
	BaseBranch     string             `json:"base_branch"`
	HeadSHA        string             `json:"head_sha"`
	State          string             `json:"state"`
	Supersedes     string             `json:"supersedes,omitempty"`
	VerifiedAt     time.Time          `json:"verified_at"`
	Provenance     EvidenceProvenance `json:"provenance"`
}
