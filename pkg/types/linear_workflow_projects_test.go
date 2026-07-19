package types

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLinearWorkflowProjectsSchemaNormalizeAndRoundTrip(t *testing.T) {
	const source = `schema_version: v1
name: adversary-labs
trigger:
  linear:
    event: status_changed
    team: ADV
    projects:
      - Adversary Labs
      - 68f0d971-0db2-4c27-b3b6-cf1f67d827a5
    states:
      - Todo
stages:
  - id: working
    entry: true
`
	var workflow WorkflowConfig
	if err := yaml.Unmarshal([]byte(source), &workflow); err != nil {
		t.Fatalf("unmarshal YAML: %v", err)
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := NormalizeWorkflowConfig(&workflow); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"Adversary Labs", "68f0d971-0db2-4c27-b3b6-cf1f67d827a5"}
	if got := workflow.Trigger.Linear.Projects; !equalStrings(got, want) {
		t.Fatalf("trigger.linear.projects = %#v, want %#v", got, want)
	}
	if got := workflow.TriggerRepos; !equalStrings(got, want) {
		t.Fatalf("normalized trigger_repos = %#v, want %#v", got, want)
	}

	encoded, err := json.Marshal(&workflow)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	if !strings.Contains(string(encoded), `"projects":["Adversary Labs","68f0d971-0db2-4c27-b3b6-cf1f67d827a5"]`) {
		t.Fatalf("JSON does not preserve trigger.linear.projects: %s", encoded)
	}
}

func TestLinearWorkflowProjectsValidation(t *testing.T) {
	for _, projects := range [][]string{{""}, {" \t "}} {
		workflow := &WorkflowConfig{
			Name: "linear-projects",
			Trigger: &WorkflowTrigger{Linear: &LinearWorkflowTrigger{
				Event:    "status_changed",
				Projects: projects,
				States:   []string{"Todo"},
			}},
		}
		err := workflow.Validate()
		if err == nil || !strings.Contains(err.Error(), "trigger.linear.projects[0] cannot be blank") {
			t.Fatalf("Validate() error = %v, want blank project error", err)
		}
	}
}

func TestLinearWorkflowProjectsMalformedListFailsParsing(t *testing.T) {
	const source = `name: malformed
trigger:
  linear:
    event: status_changed
    projects: Adversary Labs
    states: [Todo]
`
	var workflow WorkflowConfig
	if err := yaml.Unmarshal([]byte(source), &workflow); err == nil {
		t.Fatal("expected scalar trigger.linear.projects to fail parsing")
	}
}

func TestLinearWorkflowWithoutProjectsNormalizesAsUnfiltered(t *testing.T) {
	workflow := &WorkflowConfig{
		Name: "all-projects",
		Trigger: &WorkflowTrigger{Linear: &LinearWorkflowTrigger{
			Event:  "status_changed",
			States: []string{"Todo"},
		}},
	}
	if err := NormalizeWorkflowConfig(workflow); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(workflow.TriggerRepos) != 0 {
		t.Fatalf("normalized trigger_repos = %#v, want empty", workflow.TriggerRepos)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
