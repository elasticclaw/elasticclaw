package workflowv2_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	_ "modernc.org/sqlite"
)

const runtimeWorkspaceYAML = `
schema_version: 2
name: engineering
repositories:
  primary:
    provider: github
    repository: org/repo
`

const runtimeWorkflowYAML = `
schema_version: 2
name: delivery
enabled: true
initial_state: building
states:
  building:
    phase: build
    on_enter:
      effects:
        - agent.task:
            prompt: Implement the change.
  testing:
    phase: test
    invariant:
      work:
        build_complete:
          equals: true
  fixing:
    phase: build
    invariant:
      ci:
        status:
          equals: failure
    on_enter:
      effects:
        - agent.task:
            prompt: Fix the verified test failure.
  completed:
    phase: done
    terminal: true
transitions:
  build_completed:
    from: [building, fixing]
    on: agent.task.completed
    when:
      task:
        result:
          equals: success
    to: testing
    set:
      work.build_complete: true
  tests_failed:
    from: testing
    on: ci.policy.evaluated
    when:
      ci:
        status:
          equals: failure
    to: fixing
  tests_passed:
    from: testing
    on: ci.policy.evaluated
    when:
      ci:
        status:
          equals: success
    to: completed
`

func openRuntimeDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "runtime.db") + "?_txlock=immediate&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := workflowv2.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestSQLiteJSONObjectConstraintBinding(t *testing.T) {
	db := openRuntimeDB(t)
	var valid int
	var kind string
	if err := db.QueryRow(`SELECT json_valid(?), json_type(?)`, `{}`, `{}`).Scan(&valid, &kind); err != nil {
		t.Fatal(err)
	}
	if valid != 1 || kind != "object" {
		t.Fatalf("json binding valid/type = %d/%q", valid, kind)
	}
}

func createRuntimeRun(t *testing.T, store *workflowv2.Store, id string) workflowv2.Run {
	t.Helper()
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: id, TenantID: "tenant-1",
		WorkspaceYAML: []byte(runtimeWorkspaceYAML),
		WorkflowYAML:  []byte(runtimeWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func version(value uint64) *uint64 { return &value }

func TestCreateRunPinsRevisionsAndInitialEntry(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	fixed := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return fixed })

	run := createRuntimeRun(t, store, "run-create")
	if run.State != "building" || run.DisplayPhase != typesv2.PhaseBuild || run.StateVersion != 1 || run.Status != workflowv2.RunActive {
		t.Fatalf("run = %#v", run)
	}
	if len(run.WorkspaceRevision) != 64 || len(run.WorkflowRevision) != 64 {
		t.Fatalf("revisions not pinned: %#v", run)
	}
	var transitions, effects int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_transitions WHERE run_id=?`, run.ID).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_effects WHERE run_id=?`, run.ID).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if transitions != 1 || effects != 1 {
		t.Fatalf("initial transitions/effects = %d/%d, want 1/1", transitions, effects)
	}
}

func TestCreateRunAtomicallyBindsInitialAttempt(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-bound", TenantID: "tenant-1", InitialClawID: "claw-bound",
		WorkspaceYAML: []byte(runtimeWorkspaceYAML), WorkflowYAML: []byte(runtimeWorkflowYAML),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.CurrentAttemptID == "" {
		t.Fatal("run has no initial attempt")
	}
	binding, found, err := store.ActiveControlBinding(context.Background(), "tenant-1", "claw-bound")
	if err != nil || !found {
		t.Fatalf("binding found=%v err=%v", found, err)
	}
	if binding.RunID != run.ID || binding.AttemptID != run.CurrentAttemptID {
		t.Fatalf("binding = %#v, run = %#v", binding, run)
	}
	var attemptNumber int
	var status string
	if err := db.QueryRow(`SELECT number,status FROM workflow_v2_attempts WHERE id=?`, run.CurrentAttemptID).
		Scan(&attemptNumber, &status); err != nil {
		t.Fatal(err)
	}
	if attemptNumber != 1 || status != "active" {
		t.Fatalf("attempt number/status = %d/%q", attemptNumber, status)
	}
}

func TestActivationPendingBlocksEffectsUntilContextIsPinned(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-activation-pending", TenantID: "tenant-1", InitialClawID: "claw-pending",
		WorkspaceYAML: []byte(runtimeWorkspaceYAML), WorkflowYAML: []byte(runtimeWorkflowYAML),
		ActivationPending: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != workflowv2.RunSuspended || run.WaitingReason == "" {
		t.Fatalf("pending run = %#v", run)
	}
	if claim, err := store.ClaimEffect(context.Background(), "worker-before-context", time.Minute); err != nil || claim != nil {
		t.Fatalf("pre-context effect claim = %#v, %v", claim, err)
	}
	bundle, err := store.AssembleOrganizationContext(context.Background(), run.ID,
		workflowv2.KnowledgeResolverFunc(func(context.Context, workflowv2.Run, string,
			typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
			t.Fatal("workspace without knowledge sources invoked resolver")
			return typesv2.ContextBundleSource{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ID == "" {
		t.Fatal("organization context bundle was not pinned")
	}
	if err := store.CompleteActivation(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimEffect(context.Background(), "worker-after-context", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("post-context effect claim = %#v, %v", claim, err)
	}
}

func TestCancelActivationClosesAttemptAndEffects(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-activation-cancel", TenantID: "tenant-1", InitialClawID: "claw-cancel",
		WorkspaceYAML: []byte(runtimeWorkspaceYAML), WorkflowYAML: []byte(runtimeWorkflowYAML),
		ActivationPending: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelActivation(context.Background(), run.ID, "knowledge resolver unavailable"); err != nil {
		t.Fatal(err)
	}
	run, err = store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != workflowv2.RunCancelled || !strings.Contains(run.WaitingReason, "knowledge resolver") {
		t.Fatalf("cancelled run = %#v", run)
	}
	var attemptStatus, effectStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_attempts WHERE id=?`, run.CurrentAttemptID).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effects WHERE run_id=?`, run.ID).Scan(&effectStatus); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "cancelled" || effectStatus != string(workflowv2.EffectCancelled) {
		t.Fatalf("attempt/effect status = %q/%q", attemptStatus, effectStatus)
	}
	if claim, err := store.ClaimEffect(context.Background(), "worker-after-cancel", time.Minute); err != nil || claim != nil {
		t.Fatalf("cancelled effect claim = %#v, %v", claim, err)
	}
}

func TestInspectRunExplainsDurableWaitingState(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-inspect")

	inspection, err := store.InspectRun(context.Background(), "run-inspect")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.State != "building" || inspection.Run.StateVersion != 1 {
		t.Fatalf("run = %#v", inspection.Run)
	}
	if len(inspection.Waiting) != 1 || inspection.Waiting[0].Kind != "effect" {
		t.Fatalf("waiting = %#v", inspection.Waiting)
	}
	if len(inspection.ExpectedTransitions) != 1 || inspection.ExpectedTransitions[0].EventKind != "agent.task.completed" {
		t.Fatalf("expected transitions = %#v", inspection.ExpectedTransitions)
	}
	if len(inspection.Transitions) != 1 || len(inspection.Effects) != 1 || len(inspection.RecentEvents) != 1 {
		t.Fatalf("history sizes = transitions:%d effects:%d events:%d",
			len(inspection.Transitions), len(inspection.Effects), len(inspection.RecentEvents))
	}
}

func TestEffectLeaseRetryAndCompletion(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	current := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return current })
	createRuntimeRun(t, store, "run-effect")

	claim, err := store.ClaimEffect(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Effect.Status != workflowv2.EffectRunning || claim.Effect.AttemptCount != 1 {
		t.Fatalf("first claim = %#v", claim)
	}
	if err := store.CompleteEffect(context.Background(), workflowv2.CompleteEffectRequest{
		EffectID: claim.Effect.ID, AttemptID: claim.AttemptID, Worker: "worker-1",
		Status: workflowv2.EffectRetryableFailed, Error: "temporary", RetryAfter: 30 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if early, err := store.ClaimEffect(context.Background(), "worker-2", time.Minute); err != nil || early != nil {
		t.Fatalf("early claim = %#v, %v", early, err)
	}

	current = current.Add(30 * time.Second)
	retry, err := store.ClaimEffect(context.Background(), "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if retry == nil || retry.Effect.ID != claim.Effect.ID || retry.Effect.AttemptCount != 2 {
		t.Fatalf("retry claim = %#v", retry)
	}
	if err := store.CompleteEffect(context.Background(), workflowv2.CompleteEffectRequest{
		EffectID: retry.Effect.ID, AttemptID: retry.AttemptID, Worker: "worker-2",
		Status: workflowv2.EffectSucceeded, Receipt: map[string]interface{}{"external_id": "task-123"},
	}); err != nil {
		t.Fatal(err)
	}
	if again, err := store.ClaimEffect(context.Background(), "worker-3", time.Minute); err != nil || again != nil {
		t.Fatalf("claim after success = %#v, %v", again, err)
	}
}

func TestExpiredEffectLeaseBecomesUnknownWithoutAutomaticReplay(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	current := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return current })
	createRuntimeRun(t, store, "run-effect-unknown")

	claim, err := store.ClaimEffect(context.Background(), "worker-1", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	current = current.Add(time.Minute + time.Millisecond)
	next, err := store.ClaimEffect(context.Background(), "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("expired effect was automatically replayed: %#v", next)
	}
	var effectStatus, attemptStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effects WHERE id=?`, claim.Effect.ID).Scan(&effectStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effect_attempts WHERE id=?`, claim.AttemptID).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if effectStatus != string(workflowv2.EffectUnknown) || attemptStatus != string(workflowv2.EffectUnknown) {
		t.Fatalf("statuses = effect:%s attempt:%s", effectStatus, attemptStatus)
	}
	if err := store.ResolveUnknownEffect(context.Background(), workflowv2.ResolveUnknownEffectRequest{
		EffectID: claim.Effect.ID, Status: workflowv2.EffectRetryableFailed,
		Error: "provider confirms no write", RetryAfter: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if early, err := store.ClaimEffect(context.Background(), "worker-2", time.Minute); err != nil || early != nil {
		t.Fatalf("reconciled retry ran too early: %#v, %v", early, err)
	}
	current = current.Add(time.Minute)
	retry, err := store.ClaimEffect(context.Background(), "worker-2", time.Minute)
	if err != nil || retry == nil || retry.Effect.ID != claim.Effect.ID {
		t.Fatalf("reconciled retry = %#v, %v", retry, err)
	}
}

func TestApplyEventTransitionsCASAndDeduplicates(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-transition")

	input := workflowv2.EventInput{
		ID: "event-build-done", MessageID: "message-build-done",
		Kind: "agent.task.completed", ExpectedStateVersion: version(1), Producer: workflowv2.ProducerAgent,
		Payload: map[string]interface{}{"task": map[string]interface{}{"result": "success"}},
	}
	result, err := store.ApplyEvent(context.Background(), "run-transition", input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != typesv2.DispositionAccepted || result.Transition == nil || result.Run.State != "testing" || result.Run.StateVersion != 2 {
		t.Fatalf("result = %#v", result)
	}

	duplicate, err := store.ApplyEvent(context.Background(), "run-transition", input)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Disposition != typesv2.DispositionDuplicate || duplicate.Run.StateVersion != 2 {
		t.Fatalf("duplicate = %#v", duplicate)
	}
	var transitions, effects, receipts int
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_transitions WHERE run_id='run-transition'`).Scan(&transitions)
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_effects WHERE run_id='run-transition'`).Scan(&effects)
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_event_receipts WHERE run_id='run-transition'`).Scan(&receipts)
	if transitions != 2 || effects != 1 || receipts != 3 {
		t.Fatalf("transitions/effects/receipts = %d/%d/%d, want 2/1/3", transitions, effects, receipts)
	}
}

func TestStaleAndUnauthorizedEventsCannotMutateRun(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-guards")

	stale, err := store.ApplyEvent(context.Background(), "run-guards", workflowv2.EventInput{
		ID: "stale", Kind: "agent.task.completed", ExpectedStateVersion: version(9), Producer: workflowv2.ProducerAgent,
		Payload: map[string]interface{}{"task": map[string]interface{}{"result": "success"}},
	})
	if err != nil || stale.Disposition != typesv2.DispositionStaleState {
		t.Fatalf("stale = %#v, err=%v", stale, err)
	}
	unauthorized, err := store.ApplyEvent(context.Background(), "run-guards", workflowv2.EventInput{
		ID: "forged-ci", Kind: "ci.policy.evaluated", ExpectedStateVersion: version(1), Producer: workflowv2.ProducerAgent,
		Facts: map[string]interface{}{"ci.status": "success"},
	})
	if err != nil || unauthorized.Disposition != typesv2.DispositionUnauthorized {
		t.Fatalf("unauthorized = %#v, err=%v", unauthorized, err)
	}
	run, err := store.GetRun(context.Background(), "run-guards")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "building" || run.StateVersion != 1 {
		t.Fatalf("guarded run mutated: %#v", run)
	}
	var factCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_facts WHERE run_id='run-guards'`).Scan(&factCount)
	if factCount != 0 {
		t.Fatalf("forged protected facts persisted: %d", factCount)
	}
}

func TestAgentPayloadCannotSpoofProtectedEvidence(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-payload-auth")

	result, err := store.ApplyEvent(context.Background(), "run-payload-auth", workflowv2.EventInput{
		ID: "event-spoof-ci", Kind: "agent.task.completed", ExpectedStateVersion: version(1),
		Producer: workflowv2.ProducerAgent,
		Payload: map[string]interface{}{
			"task": map[string]interface{}{"result": "success"},
			"ci":   map[string]interface{}{"status": "success"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != typesv2.DispositionUnauthorized || result.Run.StateVersion != 1 {
		t.Fatalf("result = %#v", result)
	}
	inspection, err := store.InspectRun(context.Background(), "run-payload-auth")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := inspection.Facts["ci"]; exists {
		t.Fatalf("agent-created CI facts = %#v", inspection.Facts["ci"])
	}
}

func TestFeedbackLoopCreatesEffectsPerLegitimateReentry(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-loop")

	events := []workflowv2.EventInput{
		{ID: "build-1", Kind: "agent.task.completed", ExpectedStateVersion: version(1), Producer: workflowv2.ProducerAgent,
			Payload: map[string]interface{}{"task": map[string]interface{}{"result": "success"}}},
		{ID: "ci-fail", Kind: "ci.policy.evaluated", ExpectedStateVersion: version(2), Producer: workflowv2.ProducerCI,
			Facts: map[string]interface{}{"ci.status": "failure"}, Payload: map[string]interface{}{"ci": map[string]interface{}{"status": "failure"}}},
		{ID: "build-2", Kind: "agent.task.completed", ExpectedStateVersion: version(3), Producer: workflowv2.ProducerAgent,
			Payload: map[string]interface{}{"task": map[string]interface{}{"result": "success"}}},
		{ID: "ci-pass", Kind: "ci.policy.evaluated", ExpectedStateVersion: version(4), Producer: workflowv2.ProducerCI,
			Facts: map[string]interface{}{"ci.status": "success"}, Payload: map[string]interface{}{"ci": map[string]interface{}{"status": "success"}}},
	}
	for _, event := range events {
		result, err := store.ApplyEvent(context.Background(), "run-loop", event)
		if err != nil || result.Disposition != typesv2.DispositionAccepted {
			t.Fatalf("event %s result=%#v err=%v", event.ID, result, err)
		}
	}
	run, _ := store.GetRun(context.Background(), "run-loop")
	if run.State != "completed" || run.StateVersion != 5 || run.Status != workflowv2.RunCompleted {
		t.Fatalf("completed run = %#v", run)
	}
	var effects, distinctKeys int
	_ = db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT effect_key) FROM workflow_v2_effects WHERE run_id='run-loop'`).Scan(&effects, &distinctKeys)
	// Initial BUILD task + the task created by legitimate entry into fixing.
	if effects != 2 || distinctKeys != 2 {
		t.Fatalf("effects/distinct = %d/%d, want 2/2", effects, distinctKeys)
	}
}

func TestInvariantViolationSuspendsAndPersistsEvidence(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-invariant")
	_, err := store.ApplyEvent(context.Background(), "run-invariant", workflowv2.EventInput{
		ID: "build", Kind: "agent.task.completed", ExpectedStateVersion: version(1), Producer: workflowv2.ProducerAgent,
		Payload: map[string]interface{}{"task": map[string]interface{}{"result": "success"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyEvent(context.Background(), "run-invariant", workflowv2.EventInput{
		ID: "ci-unknown", Kind: "ci.policy.evaluated", ExpectedStateVersion: version(2), Producer: workflowv2.ProducerCI,
		Facts:   map[string]interface{}{"work.build_complete": nil},
		Payload: map[string]interface{}{"ci": map[string]interface{}{"status": "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// CI cannot write work.*; use an authenticated engine event to invalidate it.
	if result.Disposition != typesv2.DispositionUnauthorized {
		t.Fatalf("cross-namespace mutation = %#v", result)
	}
	result, err = store.ApplyEvent(context.Background(), "run-invariant", workflowv2.EventInput{
		ID: "engine-invalidates", Kind: "workflow.fact.changed", ExpectedStateVersion: version(2), Producer: workflowv2.ProducerEngine,
		Facts: map[string]interface{}{"work.build_complete": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != typesv2.DispositionRejected || result.Run.Status != workflowv2.RunSuspended || !strings.Contains(result.Reason, "invariant") {
		t.Fatalf("invariant result = %#v", result)
	}
}

func TestConcurrentExpectedVersionAllowsOneTransition(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-concurrent")

	start := make(chan struct{})
	results := make(chan workflowv2.EventResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"event-a", "event-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			result, err := store.ApplyEvent(context.Background(), "run-concurrent", workflowv2.EventInput{
				ID: id, Kind: "agent.task.completed", ExpectedStateVersion: version(1), Producer: workflowv2.ProducerAgent,
				Payload: map[string]interface{}{"task": map[string]interface{}{"result": "success"}},
			})
			results <- result
			errs <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	accepted, stale := 0, 0
	for result := range results {
		switch result.Disposition {
		case typesv2.DispositionAccepted:
			accepted++
		case typesv2.DispositionStaleState:
			stale++
		}
	}
	if accepted != 1 || stale != 1 {
		t.Fatalf("accepted/stale = %d/%d, want 1/1", accepted, stale)
	}
}

func TestRuntimeRecoveryDoesNotReadConversationRows(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-recovery")
	// Deliberately create and destroy an unrelated transcript-shaped table.
	if _, err := db.Exec(`CREATE TABLE messages(id TEXT PRIMARY KEY, content TEXT); INSERT INTO messages VALUES('m','[DONE]'); DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}
	restarted := workflowv2.NewStore(db)
	run, err := restarted.GetRun(context.Background(), "run-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "building" || run.StateVersion != 1 {
		t.Fatalf("conversation deletion changed recovery: %#v", run)
	}
}
