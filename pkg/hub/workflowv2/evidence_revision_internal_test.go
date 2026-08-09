package workflowv2

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	_ "modernc.org/sqlite"
)

const revisionTestWorkspace = `
schema_version: 2
name: revision-test
repositories:
  api:
    provider: github
    repository: org/api
ci:
  connections:
    github-actions:
      provider: github
  pipelines:
    github-pr:
      connection: github-actions
      repository: api
      workflow: ci.yml
`

const revisionTestWorkflow = `
schema_version: 2
name: revision-test
enabled: true
initial_state: reviewing
states:
  reviewing:
    phase: review
  done:
    phase: done
    terminal: true
transitions:
  ci_satisfied:
    from: reviewing
    on: ci.policy.evaluated
    when:
      ci:
        status:
          equals: satisfied
    to: done
ci:
  policies:
    merge-ready:
      all:
        - pipeline: github-pr
          checks: [lint]
      satisfied_for: current_pr_head
delivery:
  pull_requests:
    required: true
    minimum: 1
    ci_policy: merge-ready
`

func TestPolicyEventRejectsSupersededEvaluationRevision(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "revision.db") + "?_txlock=immediate&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db)
	if _, err := store.CreateRun(context.Background(), CreateRunRequest{
		ID: "run-policy-revision", TenantID: "tenant-1", WorkspaceYAML: []byte(revisionTestWorkspace),
		WorkflowYAML: []byte(revisionTestWorkflow),
	}); err != nil {
		t.Fatal(err)
	}
	firstAttempt, err := store.StartAttempt(context.Background(), "run-policy-revision", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := db.Exec(`INSERT INTO workflow_v2_delivery_prs(
		id,run_id,url,repository_name,repository,pr_number,source_branch,base_branch,current_head_sha,state,
		active,provenance_json,verified_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'open',1,'{}',?,?)`,
		"pr-1", "run-policy-revision", "https://github.example/org/api/pull/1", "api", "org/api", 1,
		"feature", "main", "head-1", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_delivery_heads(id,pr_id,head_sha,generation,observed_at)
		VALUES('head-generation-1','pr-1','head-1',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_evidence(
		id,run_id,pr_id,head_sha,head_generation,domain,connection,external_id,kind,status,payload_json,provenance_json,observed_at)
		VALUES('evidence-1','run-policy-revision','pr-1','head-1',1,'ci','github-actions','lint-1','lint','success',
		'{"pipeline":"github-pr"}','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	evaluated, err := store.EvaluateDeliveryPolicy(context.Background(), "run-policy-revision")
	if err != nil {
		t.Fatal(err)
	}
	if !evaluated.CISatisfied || evaluated.Revision == "" {
		t.Fatalf("evaluated policy = %#v", evaluated)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_delivery_prs SET current_head_sha='head-2',updated_at=? WHERE id='pr-1'`, now+1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_delivery_heads(id,pr_id,head_sha,generation,observed_at)
		VALUES('head-generation-2','pr-1','head-2',2,?)`, now+1); err != nil {
		t.Fatal(err)
	}
	_, err = store.applyDeliveryPolicyEvent(context.Background(), "run-policy-revision", evaluated, EventInput{
		ID: "stale-policy-event", Kind: "ci.policy.evaluated", Producer: ProducerCI,
		Payload: map[string]interface{}{"ci": map[string]interface{}{"policy": "merge-ready", "status": "satisfied"}},
		Facts:   map[string]interface{}{"ci.policy": "merge-ready", "ci.status": "satisfied"},
		Provenance: typesv2.EvidenceProvenance{
			Producer: string(ProducerCI), ObservedAt: time.UnixMilli(now).UTC(),
		},
	})
	if !errors.Is(err, errDeliveryPolicyChanged) {
		t.Fatalf("stale policy event error = %v", err)
	}
	var eventCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE id='stale-policy-event'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(context.Background(), "run-policy-revision")
	if err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 || run.State != "reviewing" {
		t.Fatalf("stale event count/run = %d/%#v", eventCount, run)
	}
	current, err := store.EvaluateDeliveryPolicy(context.Background(), "run-policy-revision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(context.Background(), "run-policy-revision", "claw-2"); err != nil {
		t.Fatal(err)
	}
	_, err = store.applyDeliveryPolicyEvent(context.Background(), "run-policy-revision", current, EventInput{
		ID: "revoked-policy-event", Kind: "workflow.delivery.evaluated", AttemptID: firstAttempt.ID,
		Producer: ProducerEngine,
		Payload:  map[string]interface{}{"workflow": map[string]interface{}{"delivery": current}},
		Facts:    map[string]interface{}{"delivery.satisfied": current.Satisfied},
		Provenance: typesv2.EvidenceProvenance{
			Producer: string(ProducerEngine), ObservedAt: time.UnixMilli(now + 1).UTC(),
		},
	})
	if err == nil {
		t.Fatal("policy event from revoked attempt was accepted")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE id='revoked-policy-event'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("revoked policy event rows = %d", eventCount)
	}
}
