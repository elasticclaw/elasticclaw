package hub

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type factoryMCPBearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t factoryMCPBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

func factoryMCPFixture(t *testing.T) (*Server, createdAPIToken) {
	t.Helper()
	s := newAPITokenTestServer(t)
	if _, err := s.db.Exec(`UPDATE tenants SET token='hub-token' WHERE id='test-tenant-id'`); err != nil {
		t.Fatal(err)
	}
	token := createAPITokenForTest(t, s, "alice", `{"name":"MCP tests"}`)
	if _, err := s.db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('other-tenant','other','other-token','other-claw-token',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	for _, claw := range []struct{ id, tenant string }{{"mcp-claw", "test-tenant-id"}, {"other-claw", "other-tenant"}} {
		if _, err := s.db.Exec(`INSERT INTO claws(id,tenant_id,name,status,tags,created_at) VALUES(?,?,?,'offline','[]',datetime('now'))`, claw.id, claw.tenant, claw.id); err != nil {
			t.Fatal(err)
		}
	}
	return s, token
}

func factoryMCPClient(t *testing.T, s *Server, token string) (*mcp.ClientSession, context.Context) {
	t.Helper()
	endpoint := httptest.NewServer(s.Handler())
	t.Cleanup(endpoint.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "factory-tests", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint.URL + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: factoryMCPBearerTransport{token, endpoint.Client().Transport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, ctx
}

func factoryMCPCall(t *testing.T, ctx context.Context, session *mcp.ClientSession, tool string, args any, wantError bool) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != wantError {
		t.Fatalf("%s: IsError=%v, content=%v", tool, result.IsError, result.Content)
	}
	return result
}

func factoryMCPText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("expected one text content: %#v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text, got %T", result.Content[0])
	}
	return text.Text
}

func TestFactoryMCPAuthentication(t *testing.T) {
	for _, scenario := range []string{"missing", "revoked", "removed-admin", "expired", "query", "cookie", "web-session", "hub-token", "github-session"} {
		t.Run(scenario, func(t *testing.T) {
			s, token := factoryMCPFixture(t)
			path := "/mcp"
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			request.Header.Set("Authorization", "Bearer "+token.Token)
			switch scenario {
			case "missing":
				request.Header.Del("Authorization")
			case "revoked":
				if _, err := s.db.Exec(`UPDATE api_tokens SET revoked_at=1 WHERE id=?`, token.ID); err != nil {
					t.Fatal(err)
				}
			case "removed-admin":
				s.mu.Lock()
				s.hubCfg.Auth.Access.Admins = []string{"bob"}
				s.mu.Unlock()
			case "expired":
				if _, err := s.db.Exec(`UPDATE api_tokens SET expires_at=1 WHERE id=?`, token.ID); err != nil {
					t.Fatal(err)
				}
			case "query":
				request.Header.Del("Authorization")
				request.URL.RawQuery = url.Values{"token": {token.Token}}.Encode()
			case "cookie":
				request.Header.Del("Authorization")
				request.AddCookie(&http.Cookie{Name: "token", Value: token.Token})
			case "web-session":
				request.Header.Del("Authorization")
				request.Header.Set(webSessionHeader, token.Token)
			case "hub-token":
				request.Header.Set("Authorization", "Bearer hub-token")
			case "github-session":
				signed, err := signGitHubSession("api-token-test-secret", "alice", "", "")
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+signed)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, request)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" || w.Body.Len() != 0 {
				t.Fatalf("auth response: %d %v %q", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestFactoryMCPRejectsTokenOnREST(t *testing.T) {
	s, token := factoryMCPFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	r.Header.Set("Authorization", "Bearer "+token.Token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("REST accepted API token: %d", w.Code)
	}
}

func TestFactoryMCPToolsAndCalls(t *testing.T) {
	s, token := factoryMCPFixture(t)
	session, ctx := factoryMCPClient(t, s, token.Token)
	init := session.InitializeResult()
	if init.ServerInfo.Name != "elasticclaw-factory" || init.ServerInfo.Version != Version || init.Instructions == "" {
		t.Fatalf("unexpected implementation: %+v", init)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Fields(`list_claws get_claw get_claw_timeline get_claw_activity get_claw_prs get_gateway_diagnostics list_workspaces list_workflows list_workflow_runs get_workflow_run get_workflow_run_logs list_workflow_run_attempts get_workflow_attempt_logs analytics_summary analytics_general_stats analytics_effectiveness analytics_runs analytics_run analytics_tickets analytics_costs analytics_cost_drivers analytics_filter_options send_message trigger_workflow update_claw kill_claw`)
	if len(listed.Tools) != len(names) {
		t.Fatalf("tools=%d want=%d", len(listed.Tools), len(names))
	}
	found := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		found[tool.Name] = tool
	}
	for _, name := range names {
		tool := found[name]
		if tool == nil {
			t.Errorf("missing %s", name)
			continue
		}
		interact := name == "send_message" || name == "trigger_workflow" || name == "update_claw" || name == "kill_claw"
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint == interact || tool.Annotations.IdempotentHint != (name == "update_claw") || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != (name == "kill_claw") {
			t.Errorf("%s annotations: %+v", name, tool.Annotations)
		}
		if name == "kill_claw" && !strings.Contains(strings.ToLower(tool.Description), "irreversibl") {
			t.Error("kill description must explain irreversibility")
		}
	}
	result := factoryMCPCall(t, ctx, session, "list_claws", map[string]any{}, false)
	var claws []types.Claw
	if err := json.Unmarshal([]byte(factoryMCPText(t, result)), &claws); err != nil {
		t.Fatal(err)
	}
	if len(claws) != 1 || claws[0].ID != "mcp-claw" {
		t.Fatalf("tenant visibility: %+v", claws)
	}
	result = factoryMCPCall(t, ctx, session, "list_claws", map[string]any{"status": "error", "limit": 1}, false)
	if strings.TrimSpace(factoryMCPText(t, result)) != "[]" {
		t.Fatal("status filter ignored")
	}
	for _, id := range []string{"unknown", "other-claw"} {
		result = factoryMCPCall(t, ctx, session, "get_claw", map[string]any{"claw_id": id}, true)
		if !strings.Contains(factoryMCPText(t, result), "HTTP 404") {
			t.Fatal("missing HTTP status")
		}
	}
	factoryMCPCall(t, ctx, session, "get_gateway_diagnostics", map[string]any{"claw_id": "../mcp-claw"}, true)
	factoryMCPCall(t, ctx, session, "get_gateway_diagnostics", map[string]any{"claw_id": "other-claw"}, true)
	factoryMCPCall(t, ctx, session, "get_gateway_diagnostics", map[string]any{"claw_id": "unknown"}, true)
	content := strings.Repeat("é", 300)
	factoryMCPCall(t, ctx, session, "send_message", map[string]any{"claw_id": "mcp-claw", "content": content}, false)
	var stored, login, tenant string
	if err := s.db.QueryRow(`SELECT content,user_login,tenant_id FROM messages WHERE claw_id='mcp-claw' AND role='user'`).Scan(&stored, &login, &tenant); err != nil {
		t.Fatal(err)
	}
	if stored != content || login != "alice" || tenant != "test-tenant-id" {
		t.Fatalf("persisted message: %q %q %q", stored, login, tenant)
	}
	factoryMCPCall(t, ctx, session, "update_claw", map[string]any{"claw_id": "mcp-claw", "name": "Renamed", "tags": []string{}}, false)
	var name string
	if err := s.db.QueryRow(`SELECT name FROM claws WHERE id='mcp-claw'`).Scan(&name); err != nil || name != "Renamed" {
		t.Fatalf("update: %q %v", name, err)
	}
	// Match existing delete-handler tests: an offline claw without a provider ID
	// exercises deletion without provisioning or contacting a real sandbox.
	factoryMCPCall(t, ctx, session, "kill_claw", map[string]any{"claw_id": "mcp-claw"}, false)
	var status string
	if err := s.db.QueryRow(`SELECT status FROM claws WHERE id='mcp-claw'`).Scan(&status); err != nil || status != "deleted" {
		t.Fatalf("delete handler: %q %v", status, err)
	}
	rows, err := s.db.Query(`SELECT owner_login,tool,args_summary,status,error FROM api_token_audit WHERE token_id=? ORDER BY id`, token.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count, successes, failures := 0, 0, 0
	for rows.Next() {
		var owner, tool, summary, status, detail string
		if err := rows.Scan(&owner, &tool, &summary, &status, &detail); err != nil {
			t.Fatal(err)
		}
		count++
		if owner != "alice" || len(summary) > 1024 {
			t.Errorf("invalid audit: %q %q", owner, summary)
		}
		switch status {
		case "ok":
			successes++
			if detail != "" {
				t.Error("success with error detail")
			}
		case "error":
			failures++
			if detail == "" {
				t.Error("error without detail")
			}
		default:
			t.Errorf("unexpected status %q", status)
		}
		if tool == "send_message" {
			var args mcpSendInput
			if err := json.Unmarshal([]byte(summary), &args); err != nil {
				t.Fatal(err)
			}
			if len([]rune(args.Content)) != 200 {
				t.Fatalf("audit content length=%d", len([]rune(args.Content)))
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 10 || successes != 5 || failures != 5 {
		t.Fatalf("audit count=%d ok=%d error=%d", count, successes, failures)
	}
}

func TestFactoryMCPRevocationAfterConnect(t *testing.T) {
	s, token := factoryMCPFixture(t)
	session, ctx := factoryMCPClient(t, s, token.Token)
	factoryMCPCall(t, ctx, session, "list_claws", map[string]any{}, false)
	if _, err := s.db.Exec(`UPDATE api_tokens SET revoked_at=1 WHERE id=?`, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_claws", Arguments: map[string]any{}}); err == nil {
		t.Fatal("revoked token reused MCP session")
	}
}

func TestFactoryMCPResponseLimitsAndAuditSummary(t *testing.T) {
	large := strings.Repeat("é", 220*1024)
	result := mcpHTTPResult(200, []byte(large))
	text := factoryMCPText(t, result)
	if result.IsError || !utf8.ValidString(text) || !strings.Contains(text, "narrow filters or use limit") || len(text) > 200*1024+100 {
		t.Fatal("invalid success truncation")
	}
	result = mcpHTTPResult(403, []byte(large))
	text = factoryMCPText(t, result)
	if !result.IsError || !strings.HasPrefix(text, "HTTP 403:") || len(text) > 2048+10 || !utf8.ValidString(text) {
		t.Fatal("invalid error truncation")
	}
	raw, _ := json.Marshal(map[string]any{"input": map[string]any{"content": strings.Repeat("x", 300)}})
	summary := mcpArgsSummary(raw)
	if strings.Contains(summary, strings.Repeat("x", 201)) || !json.Valid([]byte(summary)) {
		t.Fatalf("nested content not shortened: %s", summary)
	}
	raw, _ = json.Marshal(map[string]string{"name": large})
	if summary := mcpArgsSummary(raw); len(summary) > 1024 || !utf8.ValidString(summary) {
		t.Fatal("audit summary exceeds cap")
	}
}

func TestFactoryMCPGatewayDiagnosticsFiles(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "diagnostics")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour)
	for i, item := range []struct{ name, content string }{{"mcp-claw-old-gateway.log", "old-tail"}, {"mcp-claw-new-bridge.log", "new-tail"}, {"other-claw-new-gateway.log", "private"}} {
		path := filepath.Join(dir, item.name)
		if err := os.WriteFile(path, []byte(item.content), 0600); err != nil {
			t.Fatal(err)
		}
		modified := stamp.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(dataDir, "secret")
	if err := os.WriteFile(outside, []byte("must not leak"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "mcp-claw-symlink.log")); err != nil {
		t.Fatal(err)
	}
	files, err := readMCPGatewayDiagnostics(dataDir, "mcp-claw", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Tail != "new-tail" || files[1].Tail != "tail" {
		t.Fatalf("tails: %+v", files)
	}
	for _, id := range []string{"../secret", "a/b", `a\b`, ".", "..", ""} {
		if _, err := readMCPGatewayDiagnostics(dataDir, id, 12); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
}

func TestFactoryMCPAuditSchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE api_token_audit`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_token_audit`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list('api_token_audit') WHERE name='idx_api_token_audit_created_at'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit index=%d error=%v", count, err)
	}
}

// The standard deployment terminates TLS in Caddy, which preserves the public
// Host while forwarding requests to the hub's loopback HTTP listener.
func TestFactoryMCPBehindReverseProxy(t *testing.T) {
	s, token := factoryMCPFixture(t)
	for _, authenticated := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "http://factory.example.com/mcp", nil)
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}))
		if authenticated {
			r.Header.Set("Authorization", "Bearer "+token.Token)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		want := http.StatusUnauthorized
		if authenticated {
			want = http.StatusMethodNotAllowed
		} // Stateless GET.
		if w.Code != want {
			t.Fatalf("authenticated=%v: status=%d want=%d body=%s", authenticated, w.Code, want, w.Body.String())
		}
	}
}

func TestFactoryMCPAuditsInvalidArguments(t *testing.T) {
	s, token := factoryMCPFixture(t)
	session, ctx := factoryMCPClient(t, s, token.Token)
	// Schema validation can return a protocol error or an error tool result;
	// both must be audited even though the typed handler is never called.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"claw_id": "mcp-claw"}})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatal("missing content accepted")
	}
	var tool, status string
	if err := s.db.QueryRow(`SELECT tool,status FROM api_token_audit WHERE token_id=?`, token.ID).Scan(&tool, &status); err != nil {
		t.Fatal(err)
	}
	if tool != "send_message" || status != "error" {
		t.Fatalf("invalid argument audit: %q %q", tool, status)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_token_audit WHERE token_id=?`, token.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count=%d error=%v", count, err)
	}
}

func TestFactoryMCPKillClawRequiresExactID(t *testing.T) {
	s, token := factoryMCPFixture(t)
	// Recreate the target after a prefix-sharing claw so table order cannot
	// accidentally select the exact match first.
	if _, err := s.db.Exec(`DELETE FROM claws WHERE id='mcp-claw'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mcp-claw-sibling", "mcp-claw"} {
		if _, err := s.db.Exec(`INSERT INTO claws(id,tenant_id,name,status,tags,created_at) VALUES(?,?,?,'offline','[]',datetime('now'))`, id, "test-tenant-id", id); err != nil {
			t.Fatal(err)
		}
	}
	session, ctx := factoryMCPClient(t, s, token.Token)
	statusOf := func(id string) string {
		var status string
		if err := s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	for _, prefix := range []string{"mcp", "mcp-cl", "other"} {
		result := factoryMCPCall(t, ctx, session, "kill_claw", map[string]any{"claw_id": prefix}, true)
		if !strings.Contains(factoryMCPText(t, result), "exact full agent ID") {
			t.Fatalf("prefix %q: %s", prefix, factoryMCPText(t, result))
		}
	}
	if statusOf("mcp-claw") == "deleted" || statusOf("mcp-claw-sibling") == "deleted" {
		t.Fatal("prefix killed a claw")
	}
	factoryMCPCall(t, ctx, session, "kill_claw", map[string]any{"claw_id": "mcp-claw"}, false)
	if statusOf("mcp-claw") != "deleted" || statusOf("mcp-claw-sibling") == "deleted" {
		t.Fatalf("exact kill: target=%q sibling=%q", statusOf("mcp-claw"), statusOf("mcp-claw-sibling"))
	}
}
