package v2_test

import (
	"fmt"
	"strings"
	"testing"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const validWorkflowYAML = `
schema_version: 2
name: pull-request-delivery
enabled: true
initial_state: implementing

states:
  implementing:
    description: Work is in progress.
    phase: build
  awaiting_ci:
    description: A verified pull request exists and CI is unresolved.
    phase: pr
    invariant:
      pull_request:
        state: open
  fixing:
    description: Verified evidence indicates more work is required.
    phase: build
  awaiting_review:
    description: CI policy is satisfied.
    phase: review
  ready_to_merge:
    description: Ready.
    phase: review
  completed:
    phase: done
    terminal: true
  cancelled:
    phase: done
    terminal: true

transitions:
  pr_opened:
    from: implementing
    on: pull_request.verified_open
    when:
      pull_request:
        state: open
    to: awaiting_ci

  ci_satisfied:
    from: awaiting_ci
    on: ci.policy.evaluated
    when:
      ci:
        policy: merge_ready
        status: satisfied
    to: awaiting_review

  ci_failed:
    from: awaiting_ci
    on: ci.policy.evaluated
    when:
      ci:
        policy: merge_ready
        status: unsatisfied
    to: fixing

  fixes_pushed:
    from: fixing
    on: pull_request.head_changed
    to: awaiting_ci

  review_satisfied:
    from: awaiting_review
    on: review.policy.evaluated
    when:
      review:
        policy: required_review
        status: satisfied
    to: ready_to_merge

  review_unsatisfied:
    from: awaiting_review
    on: review.policy.evaluated
    when:
      review:
        policy: required_review
        status: unsatisfied
    to: fixing

  pull_request_merged:
    from: ready_to_merge
    on: pull_request.merged
    to: completed

commands:
  cancel:
    from: [implementing, awaiting_ci, fixing, awaiting_review, ready_to_merge]
    to: cancelled
    require_reason: true

ci:
  policies:
    merge_ready:
      all:
        - pipeline: github-pr
          checks: [lint, unit-tests]
        - pipeline: depot-container
          checks: [container-build]
      satisfied_for: current_pr_head

review:
  policies:
    required_review:
      all:
        - connection: github-reviews
          approvals:
            minimum: 1
      invalidate_on_new_head: true

delivery:
  pull_requests:
    required: true
    minimum: 1
    ci_policy: merge_ready
    review_policy: required_review
    completion: all_merged

events:
  ci.run.completed:
    clauses:
      - from: awaiting_ci
        when:
          all:
            - pipeline:
                equals: depot-container
            - conclusion:
                equals: failure
        assert:
          work.ci_failure_investigation_requested: true
        effects:
          - agent.task:
              prompt: Investigate the Depot CI failure.
`

func TestParseAndValidateWorkflowValidRFCShape(t *testing.T) {
	// Structural-only first (no workspace pair) — policies not pair-checked.
	// Use workflow without policy pipeline refs for pure validate... actually
	// ValidateWorkflow does not check pipeline refs; pair does.
	resolved, err := v2.ParseAndValidateWorkflow([]byte(validWorkflowYAML))
	if err != nil {
		t.Fatalf("ParseAndValidateWorkflow: %v", err)
	}
	if resolved.Workflow.Name != "pull-request-delivery" {
		t.Fatalf("name = %q", resolved.Workflow.Name)
	}
	if resolved.Revision == "" {
		t.Fatal("expected non-empty revision")
	}
	if resolved.Workflow.States["implementing"].Phase != v2.PhaseBuild {
		t.Fatalf("phase = %q, want build", resolved.Workflow.States["implementing"].Phase)
	}
	if resolved.Workflow.Delivery.PullRequests.Minimum != 1 {
		t.Fatalf("delivery minimum = %d, want 1", resolved.Workflow.Delivery.PullRequests.Minimum)
	}
}

func TestWorkflowRejectsUnknownDisplayPhase(t *testing.T) {
	yaml := `
schema_version: 2
name: invalid-phase
initial_state: s
states:
  s:
    phase: deploy
  done:
    terminal: true
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error = %v, want unsupported phase", err)
	}
}

func TestEnabledWorkflowRequiresDisplayPhaseForEveryState(t *testing.T) {
	yaml := `
schema_version: 2
name: missing-phase
enabled: true
initial_state: s
states:
  s:
    phase: build
  done:
    terminal: true
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "phase is required") {
		t.Fatalf("error = %v, want required phase", err)
	}
}

func TestWorkflowV2RejectsTranscriptControl(t *testing.T) {
	for name, yaml := range map[string]string{
		"event": `
schema_version: 2
name: transcript-event
initial_state: s
states:
  s: {}
  done: {terminal: true}
transitions:
  bad:
    from: s
    on: message.received
    to: done
`,
		"fact": `
schema_version: 2
name: transcript-fact
initial_state: s
states:
  s: {}
  done: {terminal: true}
transitions:
  bad:
    from: s
    on: custom.signal
    when:
      conversation:
        text:
          equals: "[DONE]"
    to: done
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
			if err == nil || !strings.Contains(err.Error(), "conversation/transcript") {
				t.Fatalf("error = %v, want conversation/transcript rejection", err)
			}
		})
	}
}

func TestWorkflowV2RejectsTranscriptFactsInAgentTasks(t *testing.T) {
	workflow := `
schema_version: 2
name: transcript-agent-task
enabled: true
initial_state: build
states:
  build:
    phase: build
    on_enter:
      effects:
        - agent.task:
            prompt: Continue the work.
            include_facts: [conversation.body]
  done:
    phase: done
    terminal: true
`
	_, _, err := v2.ParseAndValidateWorkflowPair([]byte(workflow), []byte(validWorkspaceYAML))
	if err == nil || !strings.Contains(err.Error(), "conversation/transcript") {
		t.Fatalf("error = %v, want conversation/transcript rejection", err)
	}
}

func TestWorkflowV2AllowsMarkersAsInertDescription(t *testing.T) {
	yaml := `
schema_version: 2
name: inert-prose
initial_state: s
states:
  s:
    description: The agent may literally explain [DONE] here without changing state.
  done: {terminal: true}
`
	if _, err := v2.ParseAndValidateWorkflow([]byte(yaml)); err != nil {
		t.Fatalf("inert prose should remain valid: %v", err)
	}
}

func TestWorkflowDeliveryCannotDeclareRepositories(t *testing.T) {
	yaml := `
schema_version: 2
name: fixed-repositories
initial_state: s
states:
  s: {}
  done:
    terminal: true
delivery:
  pull_requests:
    required: true
    minimum: 1
    repositories: [primary]
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "field repositories not found") {
		t.Fatalf("error = %v, want strict unknown delivery repository field", err)
	}
}

func TestWorkflowRejectsUnknownDeliveryPolicy(t *testing.T) {
	yaml := `
schema_version: 2
name: bad-delivery-policy
initial_state: s
states:
  s: {}
  done:
    terminal: true
delivery:
  pull_requests:
    required: true
    minimum: 1
    ci_policy: missing
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "no ci policies") {
		t.Fatalf("error = %v, want no ci policies", err)
	}
}

func TestWorkflowRevisionStableAcrossCalls(t *testing.T) {
	a, err := v2.ParseAndValidateWorkflow([]byte(validWorkflowYAML))
	if err != nil {
		t.Fatal(err)
	}
	b, err := v2.ParseAndValidateWorkflow([]byte(validWorkflowYAML))
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision == "" || a.Revision != b.Revision {
		t.Fatalf("revisions not stable: %q vs %q", a.Revision, b.Revision)
	}
}

func TestWorkflowPairValidWithWorkspace(t *testing.T) {
	rwf, rws, err := v2.ParseAndValidateWorkflowPair([]byte(validWorkflowYAML), []byte(validWorkspaceYAML))
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if rwf.Revision == "" || rws.Revision == "" {
		t.Fatal("expected revisions for both")
	}
}

func TestWorkflowRejectsOverlappingEventClauses(t *testing.T) {
	yaml := `
schema_version: 2
name: overlap-clauses
initial_state: awaiting_ci
states:
  awaiting_ci: {}
  done:
    terminal: true
events:
  ci.run.completed:
    clauses:
      - from: awaiting_ci
        when:
          conclusion:
            equals: success
      - from: awaiting_ci
        when:
          conclusion:
            in: [success, failure]
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil {
		t.Fatal("expected overlap error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "overlapping clauses") {
		t.Fatalf("error missing overlapping clauses: %v", err)
	}
	if !strings.Contains(msg, "awaiting_ci") {
		t.Fatalf("error missing state path: %v", err)
	}
	if !strings.Contains(msg, "events.ci.run.completed.clauses") {
		t.Fatalf("error missing clause paths: %v", err)
	}
	if !strings.Contains(msg, "success") {
		t.Fatalf("error missing witness value: %v", err)
	}
}

func TestWorkflowRejectsOverlappingOutgoingTransitions(t *testing.T) {
	yaml := `
schema_version: 2
name: overlap-transitions
initial_state: awaiting_ci
states:
  awaiting_ci: {}
  good: {}
  bad: {}
  done:
    terminal: true
transitions:
  t1:
    from: awaiting_ci
    on: ci.policy.evaluated
    when:
      status:
        equals: satisfied
    to: good
  t2:
    from: awaiting_ci
    on: ci.policy.evaluated
    when:
      status:
        in: [satisfied, unsatisfied]
    to: bad
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil {
		t.Fatal("expected transition overlap error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "overlapping") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(msg, "transitions.t1") || !strings.Contains(msg, "transitions.t2") {
		t.Fatalf("error missing transition paths: %v", err)
	}
}

func TestWorkflowRejectsProtectedNamespaceWrite(t *testing.T) {
	yaml := `
schema_version: 2
name: protected-write
initial_state: s
states:
  s: {}
  done:
    terminal: true
events:
  custom.event:
    clauses:
      - from: s
        when:
          x:
            equals: 1
        assert:
          ci.conclusion: success
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "protected namespace") {
		t.Fatalf("error = %v, want protected namespace", err)
	}
	if !strings.Contains(err.Error(), "ci.conclusion") {
		t.Fatalf("error should name fact key: %v", err)
	}
}

func TestWorkflowRejectsUnknownPipelineRef(t *testing.T) {
	yaml := `
schema_version: 2
name: unknown-pipe
initial_state: s
states:
  s: {}
  done:
    terminal: true
ci:
  policies:
    merge_ready:
      all:
        - pipeline: does-not-exist
          checks: [lint]
`
	_, _, err := v2.ParseAndValidateWorkflowPair([]byte(yaml), []byte(validWorkspaceYAML))
	if err == nil || !strings.Contains(err.Error(), "unknown pipeline") {
		t.Fatalf("error = %v, want unknown pipeline", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error should name pipeline: %v", err)
	}
}

func TestWorkflowRejectsUnsupportedEvidencePolicyShape(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "operator", body: `ci:
  policies:
    bad:
      quorum:
        - pipeline: github-pr
          checks: [lint]`, want: "unsupported CI policy field"},
		{name: "empty checks", body: `ci:
  policies:
    bad:
      pipeline: github-pr
      checks: []`, want: "checks must be a non-empty list"},
		{name: "review minimum", body: `review:
  policies:
    bad:
      connection: github-reviews
      approvals:
        minimum: -1`, want: "non-negative integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `schema_version: 2
name: invalid-policy
initial_state: start
states:
  start:
    phase: plan
` + tt.body
			_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWorkflowRejectsEffectWithoutCapability(t *testing.T) {
	// github-production has trigger_run restricted to false in validWorkspaceYAML
	yaml := `
schema_version: 2
name: no-cap
initial_state: s
states:
  s:
    on_enter:
      effects:
        - ci.trigger:
            pipeline: github-pr
            subject: current_pr_head
  done:
    terminal: true
`
	_, _, err := v2.ParseAndValidateWorkflowPair([]byte(yaml), []byte(validWorkspaceYAML))
	if err == nil {
		t.Fatal("expected capability error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unsupported") && !strings.Contains(msg, "lacks capability") {
		t.Fatalf("error = %v, want unsupported/capability", err)
	}
	if !strings.Contains(msg, "github-pr") {
		t.Fatalf("error should name pipeline: %v", err)
	}
}

func TestWorkflowRejectsTerminalStateOnEnterEffects(t *testing.T) {
	yaml := `
schema_version: 2
name: terminal-effects
initial_state: s
states:
  s: {}
  done:
    terminal: true
    on_enter:
      effects:
        - agent.task:
            prompt: "done"
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "terminal states cannot have effects") {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkflowRejectsTransitionToTerminalWithEffects(t *testing.T) {
	yaml := `
schema_version: 2
name: terminal-transition-effects
initial_state: s
states:
  s: {}
  done:
    terminal: true
transitions:
  bad:
    from: s
    to: done
    effects:
      - agent.task:
          prompt: "done"
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "transitions to terminal state") {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkflowRejectsTerminalOutgoingTransition(t *testing.T) {
	yaml := `
schema_version: 2
name: terminal-out
initial_state: s
states:
  s: {}
  done:
    terminal: true
transitions:
  bad:
    from: done
    on: x
    to: s
`
	_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "terminal states cannot have outgoing") {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkflowRejectsUnsupportedPredicate(t *testing.T) {
	for _, operator := range []string{"regex", "matches", "javascript", "shell", "gte"} {
		yaml := fmt.Sprintf(`
schema_version: 2
name: bad-pred
initial_state: s
states:
  s: {}
  done:
    terminal: true
events:
  e:
    clauses:
      - from: s
        when:
          conclusion:
            %s: failure
`, operator)
		_, err := v2.ParseAndValidateWorkflow([]byte(yaml))
		if err == nil || !strings.Contains(err.Error(), "unsupported predicate operator") {
			t.Fatalf("operator %q error = %v", operator, err)
		}
	}
}

func TestWorkflowDisjointClausesAccepted(t *testing.T) {
	yaml := `
schema_version: 2
name: disjoint
initial_state: awaiting_ci
states:
  awaiting_ci: {}
  done:
    terminal: true
events:
  ci.run.completed:
    clauses:
      - from: awaiting_ci
        when:
          conclusion:
            equals: success
      - from: awaiting_ci
        when:
          conclusion:
            equals: failure
`
	if _, err := v2.ParseAndValidateWorkflow([]byte(yaml)); err != nil {
		t.Fatalf("disjoint clauses should validate: %v", err)
	}
}

func TestWorkflowV2ExecRunEffectAccepted(t *testing.T) {
	wf := `
schema_version: 2
name: exec-run
enabled: true
initial_state: build
states:
  build:
    phase: build
    on_enter:
      effects:
        - exec.run:
            command: echo hello
            timeout: 1m
  done:
    phase: done
    terminal: true
`
	if _, _, err := v2.ParseAndValidateWorkflowPair([]byte(wf), []byte(validWorkspaceYAML)); err != nil {
		t.Fatalf("valid exec.run effect should validate: %v", err)
	}
}

func TestWorkflowV2DependencyUpdateEffectAccepted(t *testing.T) {
	wf := `
schema_version: 2
name: dependency-update
enabled: true
initial_state: build
states:
  build:
    phase: build
    on_enter:
      effects:
        - dependency.update:
            ecosystems: [go]
            timeout: 1m
  done:
    phase: done
    terminal: true
`
	if _, _, err := v2.ParseAndValidateWorkflowPair([]byte(wf), []byte(validWorkspaceYAML)); err != nil {
		t.Fatalf("valid dependency.update effect should validate: %v", err)
	}
}

func TestWorkflowV2ExecRunRejectsMissingCommand(t *testing.T) {
	wf := `
schema_version: 2
name: bad-exec
enabled: true
initial_state: build
states:
  build:
    phase: build
    on_enter:
      effects:
        - exec.run:
            timeout: 1m
  done:
    phase: done
    terminal: true
`
	_, _, err := v2.ParseAndValidateWorkflowPair([]byte(wf), []byte(validWorkspaceYAML))
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("error = %v, want command missing", err)
	}
}

func TestWorkflowV2DependencyUpdateRejectsMissingEcosystems(t *testing.T) {
	wf := `
schema_version: 2
name: bad-dep
enabled: true
initial_state: build
states:
  build:
    phase: build
    on_enter:
      effects:
        - dependency.update:
            grouping: all
  done:
    phase: done
    terminal: true
`
	_, _, err := v2.ParseAndValidateWorkflowPair([]byte(wf), []byte(validWorkspaceYAML))
	if err == nil || !strings.Contains(err.Error(), "ecosystems") {
		t.Fatalf("error = %v, want ecosystems missing", err)
	}
}

func TestWorkflowV2ExecRunRejectsMissingCapability(t *testing.T) {
	ws := `
schema_version: 2
name: restricted
repositories:
  primary:
    provider: github
    repository: org/repo
execution:
  provider: daytona
  capability_restrictions:
    execute_command: false
`
	wf := `
schema_version: 2
name: restricted-wf
enabled: true
initial_state: build
states:
  build:
    phase: build
    on_enter:
      effects:
        - exec.run:
            command: echo hi
  done:
    phase: done
    terminal: true
`
	_, _, err := v2.ParseAndValidateWorkflowPair([]byte(wf), []byte(ws))
	if err == nil || !strings.Contains(err.Error(), "execute_command") {
		t.Fatalf("error = %v, want execute_command capability missing", err)
	}
}
