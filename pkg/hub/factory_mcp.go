package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ctxAPITokenKey struct{}

func (s *Server) withAPITokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.Header.Get("Authorization"))
		reject := func() { w.Header().Set("WWW-Authenticate", "Bearer"); w.WriteHeader(http.StatusUnauthorized) }
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			reject()
			return
		}
		identity, err := s.authenticateAPIToken(r.Context(), fields[1])
		if err != nil {
			reject()
			return
		}
		tenant, err := s.githubTenantIDContext(r.Context())
		if err != nil {
			reject()
			return
		}
		ctx := context.WithValue(r.Context(), ctxTenantKey{}, tenant)
		ctx = context.WithValue(ctx, ctxGitHubLoginKey{}, identity.OwnerLogin)
		ctx = context.WithValue(ctx, ctxAnalyticsAdminKey{}, true)
		ctx = context.WithValue(ctx, ctxAPITokenKey{}, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) factoryMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "elasticclaw-factory", Version: Version}, &mcp.ServerOptions{
		Instructions: "Claws are coding agents running in sandboxes. Workspaces contain workflows; triggering a workflow creates runs and attempts. Inspect agent messages, captured diagnostics, run logs, and analytics before interacting. kill_claw irreversibly terminates an agent and its sandbox.",
	})
	s.registerFactoryMCPTools(server)
	server.AddReceivingMiddleware(s.auditFactoryMCP)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true,
		// Caddy forwards the public Host to a loopback listener. Bearer token
		// authentication is enforced before the SDK handles any request.
		DisableLocalhostProtection: true,
	})
}

func mcpText(text string, failed bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: failed, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func mcpTruncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func mcpHTTPResult(status int, body []byte) *mcp.CallToolResult {
	if status < 200 || status >= 300 {
		return mcpText(fmt.Sprintf("HTTP %d: %s", status, mcpTruncate(string(body), 2*1024)), true)
	}
	text := string(body)
	if len(text) > 200*1024 {
		text = mcpTruncate(text, 200*1024) + "\n[Truncated: narrow filters or use limit to request less data.]"
	}
	return mcpText(text, false)
}

// Run the REST handler directly with the authenticated MCP context. No REST
// authentication credentials are synthesized or accepted on other routes.
func (s *Server) mcpCallHandler(ctx context.Context, handler http.HandlerFunc, method, path string, pathValues map[string]string, query url.Values, body any) *mcp.CallToolResult {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return mcpText("Invalid JSON body: "+err.Error(), true)
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(data))
	if err != nil {
		return mcpText("Invalid request: "+err.Error(), true)
	}
	r.URL.RawQuery = query.Encode()
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, value := range pathValues {
		r.SetPathValue(key, value)
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return mcpHTTPResult(w.Code, w.Body.Bytes())
}

// Receiving middleware also audits calls rejected by SDK schema validation or
// unknown tool names, which never reach a typed tool handler.
func (s *Server) auditFactoryMCP(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call.Params == nil {
			return result, err
		}
		identity, ok := ctx.Value(ctxAPITokenKey{}).(apiTokenIdentity)
		if !ok {
			return result, err
		}
		status, detail := "ok", ""
		if err != nil {
			status, detail = "error", err.Error()
		}
		if response, ok := result.(*mcp.CallToolResult); ok && response.IsError {
			status = "error"
			for _, content := range response.Content {
				if text, ok := content.(*mcp.TextContent); ok {
					detail += text.Text
				}
			}
		}
		// Cancellation must not discard the audit record after an action completed.
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, auditErr := s.db.ExecContext(auditCtx, `INSERT INTO api_token_audit(token_id,owner_login,tool,args_summary,status,error,created_at) VALUES(?,?,?,?,?,?,?)`,
			identity.TokenID, identity.OwnerLogin, call.Params.Name, mcpArgsSummary(call.Params.Arguments), status, mcpTruncate(detail, 2048), now().Unix())
		if auditErr != nil {
			log.Printf("[mcp] audit failed: %v", auditErr)
		}
		switch call.Params.Name {
		case "send_message", "trigger_workflow", "update_claw", "kill_claw":
			var args struct {
				ClawID string `json:"claw_id"`
			}
			_ = json.Unmarshal(call.Params.Arguments, &args)
			log.Printf("[mcp] %s tool=%s claw=%s status=%s", identity.OwnerLogin, call.Params.Name, mcpTruncate(strings.Map(func(r rune) rune {
				if r < 32 || r == 127 {
					return -1
				}
				return r
			}, args.ClawID), 200), status)
		}
		return result, err
	}
}

func mcpArgsSummary(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return mcpTruncate(string(raw), 1024)
	}
	var shorten func(any)
	shorten = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, item := range v {
				if key == "content" {
					if text, ok := item.(string); ok {
						chars := []rune(text)
						if len(chars) > 200 {
							v[key] = string(chars[:200])
						}
						continue
					}
				}
				shorten(item)
			}
		case []any:
			for _, item := range v {
				shorten(item)
			}
		}
	}
	shorten(value)
	data, _ := json.Marshal(value)
	return mcpTruncate(string(data), 1024)
}

var mcpClawIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func (s *Server) handleMCPGatewayDiagnostics(w http.ResponseWriter, r *http.Request) {
	clawID := r.PathValue("id")
	if !mcpClawIDPattern.MatchString(clawID) {
		jsonError(w, 400, "invalid claw_id")
		return
	}
	// This allows post-mortem diagnostics for deleted claws, using the same
	// tenant and tag visibility checks as their retained message history.
	if !s.canViewMessages(w, r, tenantFromCtx(r), clawID) {
		return
	}
	limit := parsePositiveLimit(r, 65536, 262144)
	captures, err := readMCPGatewayDiagnostics(hubDataDir(), clawID, limit)
	if err != nil {
		jsonError(w, 500, "could not read captured diagnostics")
		return
	}
	jsonOK(w, captures)
}

type mcpGatewayCapture struct {
	Filename string `json:"filename"`
	Tail     string `json:"tail"`
}

func readMCPGatewayDiagnostics(dataDir, clawID string, limit int) ([]mcpGatewayCapture, error) {
	captures := []mcpGatewayCapture{}
	if !mcpClawIDPattern.MatchString(clawID) || limit < 1 || limit > 262144 {
		return nil, fmt.Errorf("invalid diagnostics request")
	}
	directory := filepath.Join(dataDir, "diagnostics")
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return captures, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("diagnostics is not a directory")
	}
	// Root confines every open even if a file is replaced by a symlink between
	// listing and reading. Symlink entries themselves are never included.
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	openedInfo, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, err
	}
	if !os.SameFile(info, openedInfo) {
		dir.Close()
		return nil, fmt.Errorf("diagnostics directory changed while opening")
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, err
	}
	// Capture names include UTC RFC3339Nano timestamps. Sort by modification
	// time (with a deterministic name tie break), not lexical fractional seconds.
	files := []os.FileInfo{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), clawID+"-") || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			files = append(files, info)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].ModTime().Equal(files[j].ModTime()) {
			return files[i].Name() > files[j].Name()
		}
		return files[i].ModTime().After(files[j].ModTime())
	})
	remaining := limit
	for _, info := range files {
		if remaining == 0 {
			break
		}
		file, err := root.Open(info.Name())
		if err != nil {
			return nil, err
		}
		current, err := file.Stat()
		if err != nil || !current.Mode().IsRegular() {
			file.Close()
			if err != nil {
				return nil, err
			}
			continue
		}
		size := min(current.Size(), int64(remaining))
		_, err = file.Seek(-size, io.SeekEnd)
		var data []byte
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(file, size))
		}
		file.Close()
		if err != nil {
			return nil, err
		}
		remaining -= len(data)
		captures = append(captures, mcpGatewayCapture{Filename: info.Name(), Tail: strings.ToValidUTF8(string(data), "�")})
	}
	return captures, nil
}
