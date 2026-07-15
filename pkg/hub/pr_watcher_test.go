package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestPRConditionsRemainEligibleWhenTransitionFails(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-pr-conditions"
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "PR-1", "elasticclaw", "connected", "ready"); err != nil {
		t.Fatal(err)
	}
	pr := clawPR{id: "pr-conditions", clawID: clawID, prURL: "https://github.com/o/r/pull/1"}
	if _, err := db.Exec(`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at) VALUES(?,?,?,?,?,datetime('now'))`, pr.id, pr.clawID, "o/r", 1, pr.prURL); err != nil {
		t.Fatal(err)
	}
	stage := pipeline.Stage{ID: "ready"} // already claimed: transition must fail
	s.firePRConditions(pr, stage, pipelineContext{})
	s.firePRConditions(pr, stage, pipelineContext{}) // next poll remains eligible

	var fired int
	if err := db.QueryRow(`SELECT pr_conditions_fired FROM claw_prs WHERE id=?`, pr.id).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("pr_conditions_fired = %d, want 0 after failed transitions", fired)
	}
}
