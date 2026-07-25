package types

import "time"

// Claw represents an agent instance registered with the hub.
type Claw struct {
	ID                  string         `json:"id" db:"id"`
	TenantID            string         `json:"tenant_id" db:"tenant_id"`
	Name                string         `json:"name" db:"name"`
	Template            string         `json:"template" db:"template"`
	Provider            string         `json:"provider,omitempty" db:"provider"`
	ProviderID          string         `json:"provider_id,omitempty" db:"provider_id"`
	Status              InstanceStatus `json:"status" db:"status"`
	LastSeen            time.Time      `json:"last_seen" db:"last_seen"`
	CreatedAt           time.Time      `json:"created_at" db:"created_at"`
	ContextUsage        int            `json:"context_usage"`
	BootstrapStatus     string         `json:"bootstrap_status,omitempty"`
	BootstrapDiagnostic string         `json:"bootstrap_diagnostic,omitempty"`
	GitHubIssueID       string         `json:"github_issue_id,omitempty"`
	GitHubIssueURL      string         `json:"github_issue_url,omitempty"`
	PreviewPort         int            `json:"preview_port,omitempty"`
	PreviewURL          string         `json:"preview_url,omitempty"`
	PreviewLabel        string         `json:"preview_label,omitempty"`
	PreviewReady        bool           `json:"preview_ready,omitempty"`
	PreviewExpiresAt    int64          `json:"preview_expires_at,omitempty"`
	Tags                []string       `json:"tags,omitempty"`
	Color               string         `json:"color,omitempty"`
	SSHHost             string         `json:"ssh_host,omitempty"`
	SSHPort             int            `json:"ssh_port,omitempty"`
	SSHUser             string         `json:"ssh_user,omitempty"`
}

// HubMessage is a message exchanged between a claw and a user.
type HubMessage struct {
	ID        string    `json:"id" db:"id"`
	ClawID    string    `json:"claw_id" db:"claw_id"`
	TenantID  string    `json:"tenant_id" db:"tenant_id"`
	Role      string    `json:"role" db:"role"` // "user" | "claw"
	Content   string    `json:"content" db:"content"`
	Format    string    `json:"format,omitempty" db:"format"` // "pre" = preserve whitespace
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// WSMessage is the WebSocket envelope for hub<->claw and hub<->browser comms.
//
// File-transfer message types (new):
//   - "file"          hub → claw     FilePayload
//   - "file_ack"      claw → hub     FileAck
//   - "file_read"     hub → claw     {request_id, path}
//   - "file_read_resp" claw → hub    FileReadResp
type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

// FileAck is the claw-bridge's response after writing an uploaded file.
type FileAck struct {
	RequestID string `json:"request_id"`
	Path      string `json:"path"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// FileReadResp carries the bytes of a previously-uploaded file back to the
// hub so the browser can render previews in chat history.
type FileReadResp struct {
	RequestID string `json:"request_id"`
	OK        bool   `json:"ok"`
	Data      string `json:"data,omitempty"` // base64
	Error     string `json:"error,omitempty"`
}

// RegisterPayload is sent by a claw on connect.
type RegisterPayload struct {
	ClawID         string `json:"claw_id"`
	Name           string `json:"name"`
	Template       string `json:"template"`
	Token          string `json:"token"`                      // hub claw token for auth
	ModelAuthToken string `json:"model_auth_token,omitempty"` // per-claw proof for managed model credentials
	GatewayReady   *bool  `json:"gateway_ready,omitempty"`    // true once openclaw gateway session is established; nil means unknown (old bridge, assume ready)
	Channel        string `json:"channel,omitempty"`          // "status" for status channel, empty for main
}

// CheckpointCreatePayload asks a connected bridge to prepare a checkpoint.
type CheckpointCreatePayload struct {
	CheckpointID string `json:"checkpoint_id"`
	Reason       string `json:"reason"`
	HubURL       string `json:"hub_url"`
	ClawToken    string `json:"claw_token"`
}

// CheckpointFile describes one content-addressed file in a bridge checkpoint.
type CheckpointFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   int64  `json:"mode"`
}

// CheckpointPlan is sent by the bridge before uploading checkpoint blobs.
type CheckpointPlan struct {
	CheckpointID string           `json:"checkpoint_id"`
	RootSHA256   string           `json:"root_sha256"`
	Files        []CheckpointFile `json:"files"`
}

// CheckpointPlanAck tells the bridge which content blobs the hub does not have yet.
type CheckpointPlanAck struct {
	Upload []string `json:"upload"`
}

// CheckpointComplete is sent after all missing blobs have been uploaded.
type CheckpointComplete struct {
	CheckpointID string `json:"checkpoint_id"`
	RootSHA256   string `json:"root_sha256"`
	Error        string `json:"error,omitempty"`
}

type VolumeAttachPayload struct {
	RequestID  string `json:"request_id"`
	LeaseID    string `json:"lease_id"`
	ClawID     string `json:"claw_id"`
	LeaseToken string `json:"lease_token"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Mount      string `json:"mount"`
	HubURL     string `json:"hub_url"`
	ClawToken  string `json:"claw_token"`
}

type VolumeAttachAck struct {
	RequestID string `json:"request_id"`
	LeaseID   string `json:"lease_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

type VolumeSyncPayload struct {
	RequestID  string `json:"request_id"`
	LeaseID    string `json:"lease_id"`
	ClawID     string `json:"claw_id"`
	LeaseToken string `json:"lease_token"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Mount      string `json:"mount"`
	HubURL     string `json:"hub_url"`
	ClawToken  string `json:"claw_token"`
}

type VolumeSyncAck struct {
	RequestID string `json:"request_id"`
	LeaseID   string `json:"lease_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}
