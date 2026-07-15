package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestProvisionPendingClawMovesLinearIssueToWorkingStatus(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	var mutations int
	linear := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Query == "mutation($id: String!, $stateId: String!) { issueUpdate(id: $id, input: { stateId: $stateId }) { success } }" {
			mutations++
			if body.Variables["id"] != "linear-issue-id" || body.Variables["stateId"] != "working-state-id" {
				t.Errorf("mutation variables = %#v", body.Variables)
			}
			_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"issue":{"id":"linear-issue-id","team":{"states":{"nodes":[{"id":"working-state-id","name":"Working"}]}}}}}`))
	}))
	defer linear.Close()

	factory := &types.FactoryConfig{Name: "linear-factory", Integration: "linear", Workspace: "workspace", WorkingStatus: "Working"}
	s, db := newReaperTestServer(t, &types.HubConfig{
		Factories:    []*types.FactoryConfig{factory},
		Integrations: &types.IntegrationsConfig{Linear: []*types.LinearIntegrationConfig{{Workspace: "workspace", Token: "linear-token"}}},
	})
	s.linearBaseURL = linear.URL
	insertPendingLinearClaw(t, db, "pending-linear", "linear-issue-id", factory.Name)

	s.provisionPendingClaw("pending-linear")

	if mutations != 1 {
		t.Fatalf("Linear issueUpdate mutations = %d, want 1", mutations)
	}
}

func TestProvisionPendingClawDoesNotMoveIssueWithoutWorkingStatus(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	var calls int
	linear := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer linear.Close()

	factory := &types.FactoryConfig{Name: "linear-factory", Integration: "linear", Workspace: "workspace"}
	s, db := newReaperTestServer(t, &types.HubConfig{
		Factories:    []*types.FactoryConfig{factory},
		Integrations: &types.IntegrationsConfig{Linear: []*types.LinearIntegrationConfig{{Workspace: "workspace", Token: "linear-token"}}},
	})
	s.linearBaseURL = linear.URL
	insertPendingLinearClaw(t, db, "pending-no-status", "linear-issue-id", factory.Name)

	s.provisionPendingClaw("pending-no-status")

	if calls != 0 {
		t.Fatalf("Linear API calls = %d, want 0", calls)
	}
}

func insertPendingLinearClaw(t *testing.T, db *sql.DB, clawID, issueID, factoryName string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, provider, status, tags, linear_issue_id, created_at) VALUES(?, 'tenant', ?, 'noop', 'provisioning', ?, ?, ?)`, clawID, clawID, `["factory:`+factoryName+`"]`, issueID, now())
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
}
