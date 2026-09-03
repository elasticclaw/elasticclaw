package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestWorkflowV2TriggerAssemblesOrganizationContextBeforeProvisioning(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}}, "", "", "")
	workspaceYAML := `
schema_version: 2
name: context-workspace
execution:
  provider: noop
repositories:
  primary:
    provider: github
    repository: org/repo
knowledge:
  sources:
    principles:
      type: workspace_files
      scope: organization
      required: true
      paths: [ENGINEERING.md]
    repository-guidance:
      type: repository_files
      scope: repository
      paths: [AGENTS.md]
`
	workflowYAML := `
schema_version: 2
name: context-delivery
enabled: true
manual_trigger: true
initial_state: gathering
states:
  gathering:
    phase: context
  planning:
    phase: plan
    on_enter:
      effects:
        - agent.task:
            prompt: Create a plan using the context bundle.
transitions:
  context_ready:
    from: gathering
    on: context.bundle.ready
    when:
      context:
        status:
          equals: ready
    to: planning
`
	if err := saveExternalWorkspace(&types.WorkspaceConfig{Name: "context-workspace", Files: map[string]string{
		"elasticclaw-config.yaml": workspaceYAML,
		"ENGINEERING.md":          "# Engineering principles\n\nPrefer deterministic protocols.\n",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := saveExternalWorkflows("context-workspace", []*types.WorkflowConfig{{
		Name: "context-delivery", RawConfig: workflowYAML,
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/workspaces/context-workspace/workflows/context-delivery/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var created map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowV2TestClaw(t, db, created["claw_id"])
	var state, phase, sourcesJSON string
	var version uint64
	if err := db.QueryRow(`SELECT r.state,r.display_phase,r.state_version,b.sources_json
		FROM workflow_v2_runs r JOIN workflow_v2_context_bundles b ON b.id=r.context_bundle_id
		WHERE r.id=?`, created["run_id"]).Scan(&state, &phase, &version, &sourcesJSON); err != nil {
		t.Fatal(err)
	}
	if state != "planning" || phase != "plan" || version != 2 {
		t.Fatalf("state/phase/version = %q/%q/%d", state, phase, version)
	}
	if !strings.Contains(sourcesJSON, "ENGINEERING.md") ||
		!strings.Contains(sourcesJSON, "Prefer deterministic protocols") ||
		strings.Contains(sourcesJSON, "repository-guidance") {
		t.Fatalf("organization context sources = %s", sourcesJSON)
	}
	var effects int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_effects WHERE run_id=? AND kind='agent.task'`,
		created["run_id"]).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if effects != 1 {
		t.Fatalf("planning agent effects = %d", effects)
	}
}

func waitForWorkflowV2TestClaw(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "connected" {
			return
		}
		if status == "error" || status == "deleted" {
			t.Fatalf("claw %s reached unexpected status %q", clawID, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("claw %s did not finish noop provisioning", clawID)
}

func TestWorkspaceFileKnowledgeResolverRejectsMissingRequiredDocument(t *testing.T) {
	resolver := workspaceFileKnowledgeResolver(map[string]string{})
	_, err := resolver.ResolveKnowledge(t.Context(), workflowv2.Run{WorkspaceRevision: "revision"}, "principles",
		typesv2.KnowledgeSource{Type: typesv2.KnowledgeTypeWorkspaceFiles, Paths: []string{"MISSING.md"}})
	if err == nil || !strings.Contains(err.Error(), "MISSING.md") {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkflowV2ActivationFailureCancelsBoundRun(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}}, "", "", "")
	workspaceYAML := `
schema_version: 2
name: failing-context
execution:
  provider: noop
repositories:
  primary:
    provider: github
    repository: org/repo
knowledge:
  sources:
    principles:
      type: workspace_files
      scope: organization
      required: true
      paths: [MISSING.md]
`
	workflowYAML := `
schema_version: 2
name: guarded-context
enabled: true
manual_trigger: true
initial_state: gathering
states:
  gathering:
    phase: context
    invariant:
      context:
        status:
          equals: ready
    on_enter:
      effects:
        - agent.task:
            prompt: This must never execute without context.
  completed:
    phase: done
    terminal: true
transitions:
  ready:
    from: gathering
    on: context.bundle.ready
    to: completed
`
	if err := saveExternalWorkspace(&types.WorkspaceConfig{Name: "failing-context", Files: map[string]string{
		"elasticclaw-config.yaml": workspaceYAML,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := saveExternalWorkflows("failing-context", []*types.WorkflowConfig{{
		Name: "guarded-context", RawConfig: workflowYAML,
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/workspaces/failing-context/workflows/guarded-context/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var runStatus, attemptStatus, effectStatus, clawStatus string
	if err := db.QueryRow(`SELECT r.status,a.status,e.status,c.status FROM workflow_v2_runs r
		JOIN workflow_v2_attempts a ON a.id=r.current_attempt_id
		JOIN workflow_v2_effects e ON e.run_id=r.id
		JOIN claws c ON c.id=a.claw_id
		WHERE r.workspace_name='failing-context'`).Scan(&runStatus, &attemptStatus, &effectStatus, &clawStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "cancelled" || attemptStatus != "cancelled" || effectStatus != "cancelled" || clawStatus != "deleted" {
		t.Fatalf("run/attempt/effect/claw = %q/%q/%q/%q", runStatus, attemptStatus, effectStatus, clawStatus)
	}
	if err := s.drainWorkflowV2Effects(t.Context(), "test-cancelled-worker"); err != nil {
		t.Fatal(err)
	}
}
