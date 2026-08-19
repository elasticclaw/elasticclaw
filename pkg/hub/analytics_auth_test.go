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

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestAnalyticsRoutesRequireAppropriateAdminAccess(t *testing.T) {
	routes := []struct {
		path   string
		strict bool
	}{
		{path: "/api/analytics/factories", strict: true},
		{path: "/api/analytics/summary"},
		{path: "/api/analytics/costs", strict: true},
		{path: "/api/analytics/effectiveness", strict: true},
		{path: "/api/analytics/cost-drivers", strict: true},
		{path: "/api/analytics/general-stats", strict: true},
		{path: "/api/analytics/filter-options"},
		{path: "/api/analytics/runs"},
		{path: "/api/analytics/runs/"},
		{path: "/api/analytics/tickets"},
		{path: "/api/factories/example/analytics", strict: true},
	}
	assertAnalyticsRouteTableMatchesRegistrations(t, routes)

	tagScoped := newAnalyticsAuthTestServer(t, []string{"owner={user}"})
	plain := newAnalyticsAuthTestServer(t, nil)
	adminSession := analyticsAuthTestSession(t, "alice")
	tagScopedSession := analyticsAuthTestSession(t, "bob")
	plainSession := analyticsAuthTestSession(t, "eve")

	for _, route := range routes {
		route := route
		t.Run(route.path, func(t *testing.T) {
			if got := analyticsAuthTestRequest(tagScoped, route.path, tagScopedSession); got != map[bool]int{true: http.StatusForbidden, false: http.StatusOK}[route.strict] {
				t.Fatalf("tag-scoped non-admin status = %d, strict = %t", got, route.strict)
			}
			if got := analyticsAuthTestRequest(plain, route.path, plainSession); got != http.StatusForbidden {
				t.Fatalf("plain non-admin status = %d, want %d", got, http.StatusForbidden)
			}
			if got := analyticsAuthTestRequest(tagScoped, route.path, adminSession); got != http.StatusOK {
				t.Fatalf("admin status = %d, want %d", got, http.StatusOK)
			}
		})
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
