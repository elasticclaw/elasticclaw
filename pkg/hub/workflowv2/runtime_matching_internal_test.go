package workflowv2

import (
	"testing"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// Guards the dotted-fact-key fix: a transition written with a dotted predicate
// key (exec.last_run) must match nested facts loaded from the flat
// exec.last_run.succeeded fact, and listeners whose guards fail must be
// reported so the runtime can surface an accepted-without-transition reason.
func TestMatchingTransitionResolvesDottedGuardKeys(t *testing.T) {
	resolved, err := typesv2.ParseAndValidateWorkflow([]byte(`
schema_version: 2
name: dotted-guard
enabled: true
initial_state: prepare
states:
  prepare:
    phase: setup
  update_deps:
    phase: build
transitions:
  prepare_ready:
    from: prepare
    on: exec.run.completed
    when:
      exec.last_run:
        succeeded:
          equals: true
    to: update_deps
  prepare_conflicts:
    from: prepare
    on: exec.run.failed
    when:
      exec.last_run:
        exit_code:
          equals: 1
    to: update_deps
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wf := resolved.Workflow

	facts := map[string]interface{}{
		"exec": map[string]interface{}{
			"last_run": map[string]interface{}{
				"succeeded": true,
				"exit_code": float64(0),
			},
		},
	}
	name, def, count, listeners, err := matchingTransition(wf, "exec.run.completed", "prepare", facts)
	if err != nil {
		t.Fatal(err)
	}
	if name != "prepare_ready" || def == nil || count != 1 || listeners != 1 {
		t.Fatalf("match = name:%q def:%v count:%d listeners:%d", name, def, count, listeners)
	}

	// Wrong value: the listener exists but its guard must evaluate false, and
	// the listener count must still be reported for the diagnostic reason.
	name, def, count, listeners, err = matchingTransition(wf, "exec.run.completed", "prepare", map[string]interface{}{
		"exec": map[string]interface{}{
			"last_run": map[string]interface{}{"succeeded": false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "" || def != nil || count != 0 || listeners != 1 {
		t.Fatalf("no-match = name:%q def:%v count:%d listeners:%d", name, def, count, listeners)
	}
}
