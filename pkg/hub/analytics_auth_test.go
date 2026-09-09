package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestAnalyticsRoutesRequireAppropriateAdminAccess(t *testing.T) {
	routes := []struct {
		path   string
		strict bool
	}{
		{path: "/api/analytics/factories", strict: true},
		{path: "/api/analytics/summary", strict: false},
		{path: "/api/analytics/costs", strict: true},
		{path: "/api/analytics/effectiveness", strict: false},
		{path: "/api/analytics/cost-drivers", strict: true},
		{path: "/api/analytics/general-stats", strict: false},
		{path: "/api/analytics/filter-options", strict: false},
		{path: "/api/analytics/runs", strict: false},
		{path: "/api/analytics/runs/", strict: false},
		{path: "/api/analytics/tickets", strict: false},
		{path: "/api/factories/example/analytics", strict: true},
	}
	assertAnalyticsRouteTableMatchesRegistrations(t, routes)

	tagScoped := newAnalyticsAuthTestServer(t, []string{"owner={user}"})
	plain := newAnalyticsAuthTestServer(t, nil)
	adminSession := analyticsAuthTestSession(t, "alice")
	tagScopedSession := analyticsAuthTestSession(t, "bob")
	plainSession := analyticsAuthTestSession(t, "eve")

	// Strict routes stay admin-only (cost figures and factory analytics);
	// every other analytics route is readable by any authenticated user, with
	// or without a tag ACL configured.
	nonAdminWant := map[bool]int{true: http.StatusForbidden, false: http.StatusOK}
	for _, route := range routes {
		route := route
		t.Run(route.path, func(t *testing.T) {
			if got := analyticsAuthTestRequest(tagScoped, route.path, tagScopedSession); got != nonAdminWant[route.strict] {
				t.Fatalf("tag-scoped non-admin status = %d, strict = %t", got, route.strict)
			}
			if got := analyticsAuthTestRequest(plain, route.path, plainSession); got != nonAdminWant[route.strict] {
				t.Fatalf("plain non-admin status = %d, strict = %t", got, route.strict)
			}
			if got := analyticsAuthTestRequest(tagScoped, route.path, adminSession); got != http.StatusOK {
				t.Fatalf("admin status = %d, want %d", got, http.StatusOK)
			}
		})
	}

	if got := analyticsAuthTestRequest(tagScoped, "/api/analytics/summary?token="+tagScopedSession, ""); got != http.StatusUnauthorized {
		t.Fatalf("non-admin query token status = %d, want %d", got, http.StatusUnauthorized)
	}

	if got := analyticsAuthTestRequest(tagScoped, "/api/analytics/costs?token="+adminSession, ""); got != http.StatusUnauthorized {
		t.Fatalf("admin query token status = %d, want %d", got, http.StatusUnauthorized)
	}
}

func newAnalyticsAuthTestServer(t *testing.T, viewRequiresTags []string) *Server {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Auth: &types.AuthConfig{
			SessionSecret: "analytics-auth-test-secret",
			Access: &types.AccessConfig{
				Admins:           []string{"alice"},
				ViewRequiresTags: viewRequiresTags,
			},
		},
	}, "", "", "")
	return s
}

func analyticsAuthTestSession(t *testing.T, login string) string {
	t.Helper()
	session, err := signGitHubSession("analytics-auth-test-secret", login, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func analyticsAuthTestRequest(s *Server, path, session string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// Keep this table synchronized with the mux. Parsing server.go makes a newly
// registered analytics route fail this test until its access expectation is
// explicitly added above.
func assertAnalyticsRouteTableMatchesRegistrations(t *testing.T, routes []struct {
	path   string
	strict bool
}) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	registered := make(map[string]struct{})
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" {
			return true
		}
		path, ok := call.Args[0].(*ast.BasicLit)
		if !ok || path.Kind != token.STRING {
			return true
		}
		value := strings.Trim(path.Value, "\"")
		if strings.HasPrefix(value, "/api/analytics/") || value == "/api/factories/{name}/analytics" {
			registered[value] = struct{}{}
		}
		return true
	})
	want := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		path := route.path
		if path == "/api/factories/example/analytics" {
			path = "/api/factories/{name}/analytics"
		}
		want[path] = struct{}{}
	}
	if !reflect.DeepEqual(sortedAnalyticsAuthPaths(want), sortedAnalyticsAuthPaths(registered)) {
		t.Fatalf("analytics route test table and mux registrations differ: table=%v registrations=%v", sortedAnalyticsAuthPaths(want), sortedAnalyticsAuthPaths(registered))
	}
}

func sortedAnalyticsAuthPaths(paths map[string]struct{}) []string {
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

// Analytics is readable by everyone, but money is admin-only: the shared
// endpoints must strip cost fields for non-admin sessions.
func TestAnalyticsCostFieldsAreAdminOnly(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Auth: &types.AuthConfig{
			SessionSecret: "analytics-auth-test-secret",
			Access:        &types.AccessConfig{Admins: []string{"alice"}},
		},
	}, "", "", "")
	ts := time.Now().UTC().Add(-24*time.Hour).UnixMilli()
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-cost", AttemptID: "attempt-cost", ClawID: "claw-cost", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory, Factory: "bugfix",
		StartedAt: ts, FinishedAt: ts + 1000, MergedAt: ts + 1000, PRCount: 1, MergedPRCount: 1,
		TotalTokens: 1000, EstimatedCostUsd: 12.5, UsageUpdatedAt: ts + 1000,
	})

	adminSession := analyticsAuthTestSession(t, "alice")
	memberSession := analyticsAuthTestSession(t, "eve")

	for _, tc := range []struct {
		name      string
		session   string
		wantCosts bool
	}{
		{name: "admin", session: adminSession, wantCosts: true},
		{name: "member", session: memberSession, wantCosts: false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var runs taskRunAnalyticsRunsResponse
			decodeTaskRunAnalyticsAPI(t, requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", tc.session), &runs)
			if len(runs.Runs) != 1 {
				t.Fatalf("runs = %#v", runs.Runs)
			}
			if gotCost := runs.Runs[0].EstimatedCostUsd != nil; gotCost != tc.wantCosts {
				t.Fatalf("run estimatedCostUsd present = %t, want %t", gotCost, tc.wantCosts)
			}

			var detail taskRunAnalyticsRunDetailResponse
			decodeTaskRunAnalyticsAPI(t, requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-cost", tc.session), &detail)
			if gotCost := detail.Run.EstimatedCostUsd != nil; gotCost != tc.wantCosts {
				t.Fatalf("run detail estimatedCostUsd present = %t, want %t", gotCost, tc.wantCosts)
			}

			var tickets taskRunAnalyticsTicketsResponse
			decodeTaskRunAnalyticsAPI(t, requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/tickets", tc.session), &tickets)
			if len(tickets.Tickets) != 1 || len(tickets.Tickets[0].Runs) != 1 {
				t.Fatalf("tickets = %#v", tickets.Tickets)
			}
			if gotCost := tickets.Tickets[0].Cost > 0; gotCost != tc.wantCosts {
				t.Fatalf("ticket cost present = %t, want %t", gotCost, tc.wantCosts)
			}
			if gotCost := tickets.Tickets[0].Runs[0].Cost > 0; gotCost != tc.wantCosts {
				t.Fatalf("ticket run cost present = %t, want %t", gotCost, tc.wantCosts)
			}

			var effectiveness taskRunAnalyticsEffectivenessResponse
			decodeTaskRunAnalyticsAPI(t, requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/effectiveness", tc.session), &effectiveness)
			if gotCost := len(effectiveness.TopTicketsByCost) > 0; gotCost != tc.wantCosts {
				t.Fatalf("topTicketsByCost present = %t, want %t", gotCost, tc.wantCosts)
			}
			if gotCost := effectiveness.CostPerMergedPr.Average > 0; gotCost != tc.wantCosts {
				t.Fatalf("costPerMergedPr present = %t, want %t", gotCost, tc.wantCosts)
			}
		})
	}
}
