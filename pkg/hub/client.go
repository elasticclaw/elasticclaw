package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Client talks to an ElasticClaw hub.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient creates a hub API client.
func NewClient(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{}}
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub request failed: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("hub error %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// ListClaws returns all claws for the authenticated tenant.
func (c *Client) ListClaws(ctx context.Context) ([]types.Claw, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/claws", nil)
	if err != nil {
		return nil, err
	}
	var claws []types.Claw
	return claws, json.Unmarshal(data, &claws)
}

// GetClaw returns a single claw by ID.
func (c *Client) GetClaw(ctx context.Context, id string) (*types.Claw, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/claws/"+id, nil)
	if err != nil {
		return nil, err
	}
	var claw types.Claw
	return &claw, json.Unmarshal(data, &claw)
}

// KillClaw removes a claw from the hub and disconnects it.
func (c *Client) KillClaw(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/claws/"+id, nil)
	return err
}

// GetMessages returns the message history for a claw.
func (c *Client) GetMessages(ctx context.Context, clawID string) ([]types.HubMessage, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/messages/"+clawID, nil)
	if err != nil {
		return nil, err
	}
	var msgs []types.HubMessage
	return msgs, json.Unmarshal(data, &msgs)
}

// SendMessage sends a user message to a claw.
func (c *Client) SendMessage(ctx context.Context, clawID, content string) (*types.HubMessage, error) {
	data, err := c.do(ctx, http.MethodPost, "/api/messages/"+clawID, map[string]string{"content": content})
	if err != nil {
		return nil, err
	}
	var msg types.HubMessage
	return &msg, json.Unmarshal(data, &msg)
}

// CreateClaw provisions a new claw via the hub.
func (c *Client) CreateClaw(ctx context.Context, name, templateName string, tmplCfg *types.TemplateConfig, files map[string]string, env map[string]string) (*types.Claw, error) {
	req := types.CreateClawRequest{
		Name:         name,
		TemplateName: templateName,
		Provider:     tmplCfg.Provider,
		Resources:    tmplCfg.Resources,
		InstanceType: tmplCfg.InstanceType,
		Image:        tmplCfg.Image,
		TTL:          tmplCfg.TTL,
		DefaultModel: tmplCfg.DefaultModel,
		Files:        files,
		Env:          env,
		GitHub:       tmplCfg.GitHub,
		Linear:       tmplCfg.Linear,
		Snapshot:     tmplCfg.Snapshot,
	}
	data, err := c.do(ctx, http.MethodPost, "/api/claws", req)
	if err != nil {
		return nil, err
	}
	var claw types.Claw
	return &claw, json.Unmarshal(data, &claw)
}

// WatchMessages connects to the hub user WebSocket and calls onMessage/onChunk
// whenever a message or streaming chunk arrives for the given clawID. Blocks until ctx is done.
func (c *Client) WatchMessages(ctx context.Context, clawID string, onMessage func(msg types.HubMessage)) error {
	wsURL := strings.TrimRight(c.baseURL, "/")
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	}
	wsURL += "/api/ws?token=" + c.token

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("ws connect: %w", err)
	}
	defer conn.CloseNow()

	for {
		var msg types.WSMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch msg.Type {
		case "message":
			var hm types.HubMessage
			if b, err := json.Marshal(msg.Payload); err == nil {
				if err := json.Unmarshal(b, &hm); err == nil {
					if hm.ClawID == clawID && hm.Role == "claw" {
						onMessage(hm)
					}
				}
			}
		}
	}
}

// WatchStream connects to the hub WebSocket and calls onChunk for streaming
// chunks and onMessage for complete messages. Blocks until ctx is done.
func (c *Client) WatchStream(ctx context.Context, clawID string, onChunk func(chunk string), onMessage func(msg types.HubMessage)) error {
	wsURL := strings.TrimRight(c.baseURL, "/")
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	}
	wsURL += "/api/ws?token=" + c.token

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("ws connect: %w", err)
	}
	defer conn.CloseNow()

	for {
		var msg types.WSMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch msg.Type {
		case "chunk":
			if onChunk != nil {
				var c struct {
					ClawID  string `json:"claw_id"`
					Content string `json:"content"`
				}
				if b, err := json.Marshal(msg.Payload); err == nil {
					if err := json.Unmarshal(b, &c); err == nil && c.ClawID == clawID {
						onChunk(c.Content)
					}
				}
			}
		case "message":
			if onMessage != nil {
				var hm types.HubMessage
				if b, err := json.Marshal(msg.Payload); err == nil {
					if err := json.Unmarshal(b, &hm); err == nil && hm.ClawID == clawID && hm.Role == "claw" {
						onMessage(hm)
					}
				}
			}
		}
	}
}

// Login verifies a token against the hub.
func (c *Client) Login(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodPost, "/api/login", map[string]string{"token": c.token})
	if err != nil {
		return "", err
	}
	var resp map[string]string
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	return resp["tenant_id"], nil
}
