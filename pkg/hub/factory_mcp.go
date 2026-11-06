package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		// Hub failures are not credential failures: a 401 would make a valid
		// token look revoked and stop MCP clients from retrying.
		unavailable := func(err error) {
			log.Printf("[mcp] authentication unavailable: %v", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		identity, err := s.authenticateAPIToken(r.Context(), fields[1])
		if errors.Is(err, errAPITokenUnavailable) {
			unavailable(err)
			return
		}
		if err != nil {
			reject()
			return
		}
		tenant, err := s.githubTenantIDContext(r.Context())
		if err != nil {
			unavailable(err)
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
	if strings.TrimSpace(text) == "" {
		// 204 responses (update_claw, kill_claw) would otherwise look like no result.
		return mcpText(fmt.Sprintf("HTTP %d: success", status), false)
	}
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
// unknown tool names, which never reach a typed tool handler. The audit row is
// written before the call runs, so no action can complete unaudited.
func (s *Server) auditFactoryMCP(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		identity, authenticated := ctx.Value(ctxAPITokenKey{}).(apiTokenIdentity)
		if method != "tools/call" || !ok || call.Params == nil || !authenticated {
			return next(ctx, method, req)
		}
		inserted, err := s.db.ExecContext(ctx, `INSERT INTO api_token_audit(token_id,owner_login,tool,args_summary,status,created_at) VALUES(?,?,?,?,'pending',?)`,
			identity.TokenID, identity.OwnerLogin, call.Params.Name, mcpArgsSummary(call.Params.Arguments), now().Unix())
		var auditID int64
		if err == nil {
			auditID, err = inserted.LastInsertId()
		}
		if err != nil {
			log.Printf("[mcp] audit failed, refusing tool=%s: %v", call.Params.Name, err)
			return mcpText("Call refused: the hub could not record the audit entry. Try again later.", true), nil
		}
		s.pruneAPITokenAuditIfDue(now())
		result, err := next(ctx, method, req)
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
		// Cancellation must not discard the outcome after an action completed.
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, auditErr := s.db.ExecContext(auditCtx, `UPDATE api_token_audit SET status=?, error=? WHERE id=?`, status, mcpTruncate(detail, 2048), auditID); auditErr != nil {
			log.Printf("[mcp] audit outcome failed id=%d: %v", auditID, auditErr)
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

// apiTokenAuditMaxAge is how long MCP tool-call audit rows are kept. Pruning is
// independent of checkpoint retention settings and runs at most hourly from the
// audit write path, so the table is bounded whenever it grows.
const apiTokenAuditMaxAge = 90 * 24 * time.Hour

func (s *Server) pruneAPITokenAuditIfDue(at time.Time) {
	last := s.mcpAuditPrunedAt.Load()
	if at.Unix()-last < int64(time.Hour/time.Second) || !s.mcpAuditPrunedAt.CompareAndSwap(last, at.Unix()) {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM api_token_audit WHERE created_at < ?`, at.Add(-apiTokenAuditMaxAge).Unix()); err != nil {
		log.Printf("[mcp] audit prune failed: %v", err)
	}
}

// Argument names that commonly carry credentials. Their values are never
// persisted in the audit table.
var mcpSensitiveArgPattern = regexp.MustCompile(`(?i)token|secret|passw|credential|api[_-]?key|private[_-]?key|access[_-]?key|authorization|cookie`)

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
				if mcpSensitiveArgPattern.MatchString(key) {
					v[key] = "[redacted]"
					continue
				}
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
	limit := parsePositiveLimit(r, 65536, mcpDiagnosticsMaxBytes)
	captures, err := readMCPGatewayDiagnostics(hubDataDir(), clawID, limit)
	if err != nil {
		jsonError(w, 500, "could not read captured diagnostics")
		return
	}
	jsonOK(w, captures)
}

// mcpDiagnosticsMaxBytes keeps the JSON-escaped tails under the 200 KiB tool
// result cap, so the response is never cut mid-JSON.
const mcpDiagnosticsMaxBytes = 128 * 1024

type mcpGatewayCapture struct {
	Filename string `json:"filename"`
	Tail     string `json:"tail"`
}

func readMCPGatewayDiagnostics(dataDir, clawID string, limit int) ([]mcpGatewayCapture, error) {
	captures := []mcpGatewayCapture{}
	if !mcpClawIDPattern.MatchString(clawID) || limit < 1 || limit > mcpDiagnosticsMaxBytes {
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
	// Match writeGatewayLogCapture's full name format. A bare prefix check would
	// also select captures of another claw whose ID extends this one.
	captureName := regexp.MustCompile(`^` + regexp.QuoteMeta(clawID) + `-\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z-[a-z]+\.log$`)
	files := []os.FileInfo{}
	for _, entry := range entries {
		if !captureName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
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
