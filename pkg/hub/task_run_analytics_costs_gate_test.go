package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCanViewCosts(t *testing.T) {
	access := &types.AccessConfig{Admins: []string{"Alice"}, CostViewers: []string{"Carol"}}
	tests := []struct {
		name  string
		cfg   *types.AccessConfig
		login string
		want  bool
	}{
		{name: "hub token or password session", cfg: access, login: "", want: true},
		{name: "empty login with nil config", cfg: nil, login: "", want: true},
		{name: "admin login", cfg: access, login: "alice", want: true},
		{name: "cost viewer login", cfg: access, login: "carol", want: true},
		{name: "login in neither list", cfg: access, login: "bob", want: false},
		{name: "github login with nil config", cfg: nil, login: "bob", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canViewCosts(tt.cfg, tt.login); got != tt.want {
				t.Fatalf("canViewCosts(%v, %q) = %v, want %v", tt.cfg, tt.login, got, tt.want)
			}
		})
	}
}

func newTaskRunAnalyticsCostsGateServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	return NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Auth: &types.AuthConfig{
			SessionSecret: "analytics-session-secret",
			Access:        &types.AccessConfig{Admins: []string{"alice"}, CostViewers: []string{"carol"}},
		},
	}, "", "", "")
}

func githubSessionForCostsGate(t *testing.T, login string) string {
	t.Helper()
	session, err := signGitHubSession("analytics-session-secret", login, "", "")
	if err != nil {
		t.Fatalf("sign session for %s: %v", login, err)
	}
	return session
}

func TestTaskRunAnalyticsCostEndpointsRequireCapability(t *testing.T) {
	s, _ := newTaskRunAnalyticsCostsGateServer(t)
	bob := githubSessionForCostsGate(t, "bob")
	alice := githubSessionForCostsGate(t, "alice")
	carol := githubSessionForCostsGate(t, "carol")

	for _, path := range []string{"/api/analytics/costs", "/api/analytics/cost-drivers"} {
		rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, path, bob)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s for non-capable session status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 403 body is not JSON: %s", path, rr.Body.String())
		}
		if _, ok := body["error"]; !ok {
			t.Fatalf("%s 403 body missing error key: %s", path, rr.Body.String())
		}
		for caller, token := range map[string]string{"hub token": "test-token", "admin": alice, "cost viewer": carol} {
			rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, path, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s for %s status = %d, body = %s", path, caller, rr.Code, rr.Body.String())
			}
		}
	}
}

func insertTaskRunAnalyticsCostsGateRun(t *testing.T, db *sql.DB) {
	t.Helper()
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-usage", AttemptID: "attempt-usage", ClawID: "claw-usage", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "eng", Factory: "bugfix", Integration: "github", Repo: "elastic/claw", Model: "gpt-5",
		StartedAt: 1760000000000, FinishedAt: 1760000005000, PRCount: 1, MergedPRCount: 1, MergedAt: 1760000005000,
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30, EstimatedCostUsd: 1.25, UsageUpdatedAt: 1760000005000,
	})
}

var taskRunAnalyticsCostRedactedKeys = []string{"estimatedCostUsd", "inputTokens", "outputTokens", "totalTokens", "usageUpdatedAt", "model"}

func assertTaskRunCostFields(t *testing.T, run map[string]any, wantPresent bool, context string) {
	t.Helper()
	for _, key := range taskRunAnalyticsCostRedactedKeys {
		_, present := run[key]
		if key == "usageUpdatedAt" {
			// usageUpdatedAt is never emitted by the runs API; it must stay absent.
			if present {
				t.Fatalf("%s: key %q unexpectedly present: %#v", context, key, run)
			}
			continue
		}
		if present != wantPresent {
			t.Fatalf("%s: key %q present = %v, want %v (run: %#v)", context, key, present, wantPresent, run)
		}
	}
}

func TestTaskRunAnalyticsRunsCostRedaction(t *testing.T) {
	s, db := newTaskRunAnalyticsCostsGateServer(t)
	insertTaskRunAnalyticsCostsGateRun(t, db)
	bob := githubSessionForCostsGate(t, "bob")

	for caller, token := range map[string]string{"non-capable": bob, "hub token": "test-token"} {
		wantPresent := token != bob
		listRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", token)
		var list map[string]any
		decodeTaskRunAnalyticsAPI(t, listRR, &list)
		runs, ok := list["runs"].([]any)
		if !ok || len(runs) != 1 {
			t.Fatalf("%s: unexpected runs payload: %#v", caller, list)
		}
		assertTaskRunCostFields(t, runs[0].(map[string]any), wantPresent, caller+" list")

		detailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-usage", token)
		var detail map[string]any
		decodeTaskRunAnalyticsAPI(t, detailRR, &detail)
		run, ok := detail["run"].(map[string]any)
		if !ok {
			t.Fatalf("%s: unexpected detail payload: %#v", caller, detail)
		}
		assertTaskRunCostFields(t, run, wantPresent, caller+" detail")
	}
}

func TestTaskRunAnalyticsModelFilterRequiresCapability(t *testing.T) {
	s, db := newTaskRunAnalyticsCostsGateServer(t)
	insertTaskRunAnalyticsCostsGateRun(t, db)
	bob := githubSessionForCostsGate(t, "bob")

	paths := []string{
		"/api/analytics/runs?model=gpt-5",
		"/api/analytics/summary?model=gpt-5",
		"/api/analytics/effectiveness?model=gpt-5",
		"/api/analytics/general-stats?model=gpt-5",
	}
	for _, path := range paths {
		rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, path, bob)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s for non-capable session status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 403 body is not JSON: %s", path, rr.Body.String())
		}
		if _, ok := body["error"]; !ok {
			t.Fatalf("%s 403 body missing error key: %s", path, rr.Body.String())
		}
		capableRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, path, "test-token")
		if capableRR.Code != http.StatusOK {
			t.Fatalf("%s for capable caller status = %d, body = %s", path, capableRR.Code, capableRR.Body.String())
		}
	}
}

func TestTaskRunAnalyticsFilterOptionsModelRedaction(t *testing.T) {
	s, db := newTaskRunAnalyticsCostsGateServer(t)
	insertTaskRunAnalyticsCostsGateRun(t, db)
	bob := githubSessionForCostsGate(t, "bob")

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", bob)
	var body map[string]any
	decodeTaskRunAnalyticsAPI(t, rr, &body)
	if _, present := body["models"]; present {
		t.Fatalf("non-capable filter-options response should omit models: %#v", body)
	}
	if _, present := body["workflows"]; !present {
		t.Fatalf("non-capable filter-options response should keep workflows: %#v", body)
	}

	capableRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", "test-token")
	var capable map[string]any
	decodeTaskRunAnalyticsAPI(t, capableRR, &capable)
	models, ok := capable["models"].([]any)
	if !ok || len(models) != 1 || models[0] != "gpt-5" {
		t.Fatalf("capable filter-options models = %#v, want [gpt-5]", capable["models"])
	}
}

func TestTaskRunAnalyticsEffectivenessCostRedaction(t *testing.T) {
	s, db := newTaskRunAnalyticsCostsGateServer(t)
	insertTaskRunAnalyticsCostsGateRun(t, db)
	bob := githubSessionForCostsGate(t, "bob")

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/effectiveness", bob)
	var body map[string]any
	decodeTaskRunAnalyticsAPI(t, rr, &body)
	for _, key := range []string{"costPerMergedPr", "topTicketsByCost"} {
		if _, present := body[key]; present {
			t.Fatalf("non-capable effectiveness response should omit %q: %#v", key, body)
		}
	}
	for _, key := range []string{"outcomesByDay", "funnel", "mergeRate", "successRate", "uniqueTickets", "ticketSuccessRate", "ticketsByDay", "runsPerTicket", "prior"} {
		if _, present := body[key]; !present {
			t.Fatalf("non-capable effectiveness response should keep %q: %#v", key, body)
		}
	}

	capableRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/effectiveness", "test-token")
	var capable map[string]any
	decodeTaskRunAnalyticsAPI(t, capableRR, &capable)
	for _, key := range []string{"costPerMergedPr", "topTicketsByCost"} {
		if _, present := capable[key]; !present {
			t.Fatalf("capable effectiveness response should include %q: %#v", key, capable)
		}
	}
}

func TestWebMeIncludesCostsReadCapability(t *testing.T) {
	s, _ := newTaskRunAnalyticsCostsGateServer(t)
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "password/hub token session", token: "test-token", want: true},
		{name: "admin github session", token: githubSessionForCostsGate(t, "alice"), want: true},
		{name: "cost viewer github session", token: githubSessionForCostsGate(t, "carol"), want: true},
		{name: "plain github session", token: githubSessionForCostsGate(t, "bob"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/auth/me", tt.token)
			var body map[string]any
			decodeTaskRunAnalyticsAPI(t, rr, &body)
			caps, ok := body["capabilities"].(map[string]any)
			if !ok {
				t.Fatalf("capabilities missing or wrong shape: %#v", body)
			}
			if got, ok := caps["costs:read"].(bool); !ok || got != tt.want {
				t.Fatalf("capabilities[costs:read] = %#v, want %v", caps["costs:read"], tt.want)
			}
		})
	}
}
