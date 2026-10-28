package hub

import (
	"strings"
	"sync"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const scopedSignalPipeline = `stages:
  - id: working
    label: Preparation
  - id: setup
    label: Preparation
  - id: finalize
    triggers:
      - message_contains: "[NEXT]"
        from: [working, setup]
  - id: finished
    triggers:
      - message_contains: "[DONE]"
        from: finalize
      - message_contains: "[STALE]"
        from: finalize
`

func setScopedTestStage(t *testing.T, s *Server, clawID, stage string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE claws SET pipeline_stage=? WHERE id=?`, stage, clawID); err != nil {
		t.Fatal(err)
	}
}

func rejectedSignalMessages(t *testing.T, s *Server, clawID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT content FROM messages WHERE claw_id=? AND role='hub' AND content LIKE '%is not accepted in stage%' ORDER BY rowid`, clawID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var messages []string
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestScopedSignalRejectedOncePerStageAndToken(t *testing.T) {
	const clawID = "claw-scoped-rejections"
	s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{Name: "scoped", PipelineYAML: scopedSignalPipeline}})
	if s.hasPipelineMessageContainsTrigger(clawID, "[DONE]") {
		t.Fatal("out-of-stage trigger accepted")
	}
	if s.checkPipelineMessageTriggers(clawID, "[DONE]") {
		t.Fatal("out-of-stage trigger advanced")
	}
	want := "[hub] [DONE] is not accepted in stage Preparation. This stage ends with: [NEXT]."
	if got := rejectedSignalMessages(t, s, clawID); len(got) != 1 || got[0] != want {
		t.Fatalf("rejections = %v, want %q", got, want)
	}
	if _, err := s.db.Exec(`UPDATE messages SET delivered_at=1 WHERE claw_id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.nudgeRejectedPipelineSignals(clawID, "[done]\n[STALE]")
		}()
	}
	wg.Wait()
	if got := rejectedSignalMessages(t, s, clawID); len(got) != 2 {
		t.Fatalf("repeated/concurrent signals produced %d rejections, want 2", len(got))
	}
	// Two stages can have identical labels and allowed tokens, even while the
	// first stage's identical correction is still pending delivery.
	setScopedTestStage(t, s, clawID, "setup")
	s.nudgeRejectedPipelineSignals(clawID, "[DONE]\n[STALE]")
	setScopedTestStage(t, s, clawID, "working")
	s.nudgeRejectedPipelineSignals(clawID, "[DONE]\n[STALE]")
	if got := rejectedSignalMessages(t, s, clawID); len(got) != 4 {
		t.Fatalf("stage change/revisit produced %d rejections, want 4", len(got))
	}
	setScopedTestStage(t, s, clawID, "finalize")
	if !s.hasPipelineMessageContainsTrigger(clawID, "[DONE]") {
		t.Fatal("trigger rejected inside its source stage")
	}
	s.nudgeRejectedPipelineSignals(clawID, "[DONE]")
	if got := rejectedSignalMessages(t, s, clawID); len(got) != 4 {
		t.Fatalf("accepted token produced a rejection: %v", got)
	}
}

func TestScopedSignalSuppressesUnanchoredNudge(t *testing.T) {
	const clawID = "claw-scoped-unanchored"
	s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{Name: "scoped", PipelineYAML: scopedSignalPipeline}})
	s.recordPipelineStageVisit(clawID, "finished")
	message := "[STALE]\nI might send [DONE] later."
	s.checkPipelineMessageTriggers(clawID, message)
	s.nudgeUnanchoredSignal(clawID, message)
	if got := rejectedSignalMessages(t, s, clawID); len(got) != 1 || !strings.Contains(got[0], "[STALE]") {
		t.Fatalf("rejections = %v", got)
	}
	if got := nudgeMessages(t, s, clawID); len(got) != 0 {
		t.Fatalf("also sent unanchored nudges: %v", got)
	}
}

func TestScopedDoneDoesNotFallBackToLegacyCompletion(t *testing.T) {
	const clawID = "claw-scoped-done"
	s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{Name: "scoped", PipelineYAML: scopedSignalPipeline}})
	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/1")
	if got := s.getPipelineStage(clawID); got != "working" {
		t.Fatalf("stage = %q", got)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil || status != "connected" {
		t.Fatalf("status=%q, err=%v", status, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("registered PR count=%d, err=%v", count, err)
	}
	if got := rejectedSignalMessages(t, s, clawID); len(got) != 1 {
		t.Fatalf("rejections = %v", got)
	}
}

func TestScopedSignalRetryLoop(t *testing.T) {
	const clawID = "claw-scoped-retry"
	s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{Name: "retry", PipelineYAML: `stages:
  - id: working
  - id: build_check
    triggers:
      - message_contains: "[RETRY_BUILD]"
        from: fix_build
  - id: fix_build
    triggers:
      - message_contains: "[BUILD_FAILED]"
        from: build_check
`}})
	setScopedTestStage(t, s, clawID, "build_check")
	s.recordPipelineStageVisit(clawID, "build_check")
	for i := 0; i < 2; i++ {
		if !s.checkPipelineMessageTriggers(clawID, "[BUILD_FAILED]") || s.getPipelineStage(clawID) != "fix_build" {
			t.Fatal("failed to enter fix_build")
		}
		if !s.checkPipelineMessageTriggers(clawID, "[RETRY_BUILD]") || s.getPipelineStage(clawID) != "build_check" {
			t.Fatal("failed to re-enter visited build_check")
		}
	}
}

func TestScopedRunnerReadsCurrentStage(t *testing.T) {
	for _, trigger := range []string{
		"judge_verdict: pass",
		"gate_result: {stage: source, verdict: pass}",
		"output_matches: {output: build, path: status, any_of: [pass]}",
		"pr_conditions: {}",
	} {
		t.Run(trigger, func(t *testing.T) {
			const clawID = "claw-scoped-runner"
			factory := &types.FactoryConfig{Name: "scoped", PipelineYAML: "stages:\n  - id: working\n  - id: source\n  - id: target\n    triggers:\n      - " + trigger + "\n        from: source\n"}
			s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{factory})
			ctx := pipelineContext{Factory: factory}
			s.persistPipelineOutput(clawID, "source", "build", &pipelineRunResult{Stdout: `{"status":"pass"}`})
			evaluate := func() {
				switch {
				case strings.HasPrefix(trigger, "judge_verdict"):
					s.autoTransitionAfterJudge(clawID, "pass", ctx)
				case strings.HasPrefix(trigger, "gate_result"):
					s.autoTransitionAfterGate(clawID, "source", "pass", ctx)
				case strings.HasPrefix(trigger, "output_matches"):
					s.checkPipelineMessageTriggers(clawID, "ordinary message")
				case strings.HasPrefix(trigger, "pr_conditions"):
					stage, _ := s.checkPRConditions(clawPR{clawID: clawID}, "", ctx)
					if stage != nil {
						s.transitionPipelineStageWithContext(clawID, *stage, ctx)
					}
				}
			}
			evaluate()
			if got := s.getPipelineStage(clawID); got != "working" {
				t.Fatalf("out-of-stage trigger advanced to %q", got)
			}
			setScopedTestStage(t, s, clawID, "source")
			evaluate()
			if got := s.getPipelineStage(clawID); got != "target" {
				t.Fatalf("trigger did not see updated source stage: %q", got)
			}
		})
	}
}
