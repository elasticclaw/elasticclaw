package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkflowV2RunInspectionAPIIsTenantScoped(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	_, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-inspect-api",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-inspect-api", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var inspection workflowv2.Inspection
	if err := json.NewDecoder(rr.Body).Decode(&inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Run.ID != "run-inspect-api" || len(inspection.Waiting) != 1 || inspection.Waiting[0].Kind != "effect" {
		t.Fatalf("inspection = %#v", inspection)
	}

	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,datetime('now'))`,
		"other-tenant", "other", "other-token", "other-claw-token"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-inspect-api", nil)
	req.Header.Set("Authorization", "Bearer other-token")
	rr = httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestWorkflowV2RunInspectionAPIRejectsOtherMethods(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/api/v2/workflow-runs/run-id", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status/allow = %d/%q", rr.Code, rr.Header().Get("Allow"))
	}
}

const workflowV2APIWorkspace = `
schema_version: 2
name: engineering
repositories:
  primary:
    provider: github
    repository: org/repo
`

const workflowV2APIWorkflow = `
schema_version: 2
name: delivery
enabled: true
initial_state: planning
states:
  planning:
    phase: plan
    on_enter:
      effects:
        - agent.task:
            prompt: Make a plan.
  done:
    phase: done
    terminal: true
transitions:
  planned:
    from: planning
    on: agent.task.completed
    to: done
`
