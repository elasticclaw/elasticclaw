package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
)

// uploadDirPath lands files inside OpenClaw's workspace so the agent's
// vision/media tools (which allowlist $HOME/.openclaw/workspace) can read them.
func uploadDirPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".openclaw", "workspace", "uploads")
	}
	return "/tmp/elasticclaw-uploads"
}

type fileIn struct {
	RequestID string `json:"request_id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mimetype"`
	Size      int64  `json:"size"`
	Data      string `json:"data"`
}

// handleFileMessage decodes a base64 file payload from the hub and writes it
// under uploadDirPath(), then sends a file_ack back.
func handleFileMessage(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) {
	var f fileIn
	if err := json.Unmarshal(payload, &f); err != nil {
		log.Printf("[bridge] file: bad payload: %v", err)
		return
	}
	ack := types.FileAck{RequestID: f.RequestID}

	data, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		ack.Error = fmt.Sprintf("decode: %v", err)
		sendAck(ctx, conn, ack)
		return
	}

	if err := os.MkdirAll(uploadDirPath(), 0o755); err != nil {
		ack.Error = fmt.Sprintf("mkdir: %v", err)
		sendAck(ctx, conn, ack)
		return
	}

	name := sanitizeFilename(f.Filename)
	path := filepath.Join(uploadDirPath(), fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		ack.Error = fmt.Sprintf("write: %v", err)
		sendAck(ctx, conn, ack)
		return
	}

	log.Printf("[bridge] file saved: %s (%d bytes)", path, len(data))
	ack.OK = true
	ack.Path = path
	sendAck(ctx, conn, ack)
}

func sendAck(ctx context.Context, conn *websocket.Conn, ack types.FileAck) {
	_ = writeHubMessage(ctx, conn, hubMsg{
		Type:    "file_ack",
		Payload: mustJSON(ack),
	})
}

type fileReadIn struct {
	RequestID string `json:"request_id"`
	Path      string `json:"path"`
}

// handleFileReadMessage returns the bytes of a previously-uploaded file so the
// hub can serve it back to the browser (e.g. image previews in chat history).
// Restricted to uploadDirPath() to prevent arbitrary filesystem read.
func handleFileReadMessage(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) {
	var req fileReadIn
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}
	resp := types.FileReadResp{RequestID: req.RequestID}
	send := func() {
		_ = writeHubMessage(ctx, conn, hubMsg{Type: "file_read_resp", Payload: mustJSON(resp)})
	}

	dir := uploadDirPath()
	abs, err := filepath.EvalSymlinks(req.Path)
	if err != nil {
		resp.Error = "bad path"
		send()
		return
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resp.Error = "bad path"
		send()
		return
	}
	if !strings.HasPrefix(abs, resolvedDir+string(filepath.Separator)) {
		resp.Error = "path outside uploads dir"
		send()
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		resp.Error = err.Error()
		send()
		return
	}
	resp.OK = true
	resp.Data = base64.StdEncoding.EncodeToString(data)
	send()
}

// sanitizeFilename produces a shell-safe basename. Anything outside
// [A-Za-z0-9._-] is replaced with "_" so an agent that shell-executes an
// unquoted path can't be tricked into command injection via the filename.
// The display name shown in the UI still comes from the original File.name.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "upload"
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}
