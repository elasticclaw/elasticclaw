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
type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

// RegisterPayload is sent by a claw on connect.
type RegisterPayload struct {
	ClawID        string `json:"claw_id"`
	Name          string `json:"name"`
	Template      string `json:"template"`
	Token         string `json:"token"` // hub claw token for auth
	GatewayReady  bool   `json:"gateway_ready"` // true once openclaw gateway session is established
}

