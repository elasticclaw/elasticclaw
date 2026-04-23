package types

import "time"

// Claw represents an agent instance registered with the hub.
type Claw struct {
	ID           string         `json:"id" db:"id"`
	TenantID     string         `json:"tenant_id" db:"tenant_id"`
	Name         string         `json:"name" db:"name"`
	Template     string         `json:"template" db:"template"`
	Status       InstanceStatus `json:"status" db:"status"`
	LastSeen     time.Time      `json:"last_seen" db:"last_seen"`
	CreatedAt    time.Time      `json:"created_at" db:"created_at"`
	ContextUsage int            `json:"context_usage"`
	Tags         []string       `json:"tags,omitempty"`
	Color        string         `json:"color,omitempty"`
	SSHHost      string         `json:"ssh_host,omitempty"`
	SSHPort      int            `json:"ssh_port,omitempty"`
	SSHUser      string         `json:"ssh_user,omitempty"`
}

// HubMessage is a message exchanged between a claw and a user.
type HubMessage struct {
	ID             string    `json:"id" db:"id"`
	ClawID         string    `json:"claw_id" db:"claw_id"`
	TenantID       string    `json:"tenant_id" db:"tenant_id"`
	Role           string    `json:"role" db:"role"` // "user" | "claw"
	Content        string    `json:"content" db:"content"`
	CreatedAt      time.Time `json:"created_at" db:"created_at"`
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
	ClawID        string `json:"claw_id"`
	Name          string `json:"name"`
	Template      string `json:"template"`
	Token         string `json:"token"` // hub claw token for auth
	GatewayReady  *bool  `json:"gateway_ready,omitempty"` // true once openclaw gateway session is established; nil means unknown (old bridge, assume ready)
}

