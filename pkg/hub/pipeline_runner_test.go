package hub

import (
	"sync"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestTransitionPipelineStageSkipsDuplicateCurrentStage(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-duplicate-stage"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "MAR-56", "elasticclaw", "connected", "pr_opened",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{
		ID:    "pr_opened",
		Label: "PR Opened",
		OnEnter: pipeline.OnEnter{
			Inject: "PR created. Watch for CI results and review comments.",
		},
	}
	if s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{}) {
		t.Fatalf("duplicate stage transition returned true")
	}

	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("duplicate stage transition injected %d messages, want 0", messageCount)
	}
	if got := s.getPipelineStage(clawID); got != "pr_opened" {
		t.Fatalf("pipeline stage = %q, want pr_opened", got)
	}
}

func TestTransitionPipelineStageConcurrentCallsRunOnEnterOnce(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-concurrent-stage"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "MAR-56", "elasticclaw", "connected", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{
		ID:    "pr_opened",
		Label: "PR Opened",
		OnEnter: pipeline.OnEnter{
			Inject: "PR created. Watch for CI results and review comments.",
		},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	transitionCount := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{}) {
				mu.Lock()
				transitionCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if transitionCount != 1 {
		t.Fatalf("concurrent stage transitions returned true %d times, want 1", transitionCount)
	}

	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 1 {
		t.Fatalf("concurrent stage transitions injected %d messages, want 1", messageCount)
	}
	if got := s.getPipelineStage(clawID); got != "pr_opened" {
		t.Fatalf("pipeline stage = %q, want pr_opened", got)
	}
}
