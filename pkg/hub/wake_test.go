package hub

import (
	"strings"
	"testing"
)

func TestInitialPlanWakePromptRequiresVisiblePlanBeforeWork(t *testing.T) {
	required := []string{
		"Initial plan required before implementation",
		"Before editing files, running builds, or doing broad tool exploration",
		"Your understanding of the issue or task",
		"The likely area of the codebase or behavior involved",
		"A rough implementation plan",
		"What you will verify or test",
		"Tool calls, activity rows, and update_plan do not count",
		"wait for the hub's proceed message",
	}
	for _, want := range required {
		if !strings.Contains(initialPlanWakeContent, want) {
			t.Fatalf("initial plan wake content missing %q:\n%s", want, initialPlanWakeContent)
		}
	}
}

func TestIsValidInitialPlanRequiresUnderstandingPlanAreaAndVerification(t *testing.T) {
	valid := `I understand the issue is that automated workflow agents can spend too long working before the user sees a useful summary. The likely code area is the hub startup and message handling code, especially the wake prompt and bridge or server files that manage workflow claws. My plan is to add an initial planning checkpoint, persist its state, validate the first visible assistant message, and only then send a proceed instruction. I will verify this with focused hub tests and a package test run.`
	if !isValidInitialPlan(valid) {
		t.Fatalf("valid initial plan was rejected")
	}
	invalid := "Good, build passes. Now let me read the existing test files."
	if isValidInitialPlan(invalid) {
		t.Fatalf("invalid initial plan was accepted")
	}
}

func TestHandleInitialPlanResponseMarksAcceptedOrCorrection(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-plan", "test-tenant-id", "claw plan", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	s.insertSystemMarker("claw-plan", "test-tenant-id", initialPlanRequiredMarker)
	s.handleInitialPlanResponse("claw-plan", "test-tenant-id", "Good, build passes. Now let me read the existing test files.")
	if !s.hasSystemMarker("claw-plan", initialPlanCorrectionSentMarker) {
		t.Fatalf("invalid initial plan did not mark correction sent")
	}
	if s.hasSystemMarker("claw-plan", initialPlanAcceptedMarker) {
		t.Fatalf("invalid initial plan was accepted")
	}

	_, err = db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-valid-plan", "test-tenant-id", "claw valid plan", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	s.insertSystemMarker("claw-valid-plan", "test-tenant-id", initialPlanRequiredMarker)
	valid := `I understand the issue is that the agent is not reliably sending a visible plan before it starts implementation. The likely code area is the hub server message flow and workflow wake handling code. My plan is to add persisted plan-required state, validate the first assistant message, and send a proceed instruction only after the plan is accepted. I will verify the change with focused server tests and the hub package tests.`
	s.handleInitialPlanResponse("claw-valid-plan", "test-tenant-id", valid)
	if !s.hasSystemMarker("claw-valid-plan", initialPlanAcceptedMarker) {
		t.Fatalf("valid initial plan was not accepted")
	}
	if s.hasSystemMarker("claw-valid-plan", initialPlanCorrectionSentMarker) {
		t.Fatalf("valid initial plan marked correction sent")
	}
}

func TestHandleInitialPlanActivityMarksCorrectionOnToolBeforePlan(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-tool-before-plan", "test-tenant-id", "claw tool before plan", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	s.insertSystemMarker("claw-tool-before-plan", "test-tenant-id", initialPlanRequiredMarker)
	s.handleInitialPlanActivity("claw-tool-before-plan", "test-tenant-id", map[string]interface{}{"kind": "tool", "tool": "exec"})
	if !s.hasSystemMarker("claw-tool-before-plan", initialPlanCorrectionSentMarker) {
		t.Fatalf("tool activity before initial plan did not mark correction sent")
	}
}

func TestInsertSystemMarkerReportsOnlyFirstInsert(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-marker", "test-tenant-id", "claw marker", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !s.insertSystemMarker("claw-marker", "test-tenant-id", initialPlanRequiredMarker) {
		t.Fatalf("first marker insert returned false")
	}
	if s.insertSystemMarker("claw-marker", "test-tenant-id", initialPlanRequiredMarker) {
		t.Fatalf("duplicate marker insert returned true")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='system' AND content=?`, "claw-marker", initialPlanRequiredMarker).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one marker row, got %d", count)
	}
}
