package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLinearProjectMatches(t *testing.T) {
	tests := []struct {
		name        string
		projectID   string
		projectName string
		configured  []string
		want        bool
	}{
		{name: "omitted filter matches project", projectName: "Anything", want: true},
		{name: "empty filter matches no project", configured: []string{}, want: true},
		{name: "configured filter rejects no project", configured: []string{"Adversary Labs"}, want: false},
		{name: "stable ID", projectID: "68f0d971-0db2-4c27-b3b6-cf1f67d827a5", projectName: "Renamed", configured: []string{"68f0d971-0db2-4c27-b3b6-cf1f67d827a5"}, want: true},
		{name: "name ignores case and whitespace", projectName: " Adversary Labs ", configured: []string{"  adversary labs  "}, want: true},
		{name: "multiple projects use OR", projectName: "Adversary Labs", configured: []string{"Platform", "Adversary Labs", "Website"}, want: true},
		{name: "different project", projectName: "Platform", configured: []string{"Adversary Labs"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := linearProjectMatches(test.projectID, test.projectName, test.configured); got != test.want {
				t.Fatalf("linearProjectMatches(%q, %q, %#v) = %v, want %v", test.projectID, test.projectName, test.configured, got, test.want)
			}
		})
	}
}

func TestLinearWorkflowFiltersComposeWithANDSemantics(t *testing.T) {
	workflow := &types.WorkflowConfig{
		Team:          "ADV",
		TriggerStatus: "Todo",
		Labels:        []string{"agent-ready"},
		ExcludeLabels: []string{"blocked"},
		AssignedTo:    "@Marc",
		Trigger: &types.WorkflowTrigger{Linear: &types.LinearWorkflowTrigger{
			Projects: []string{"Adversary Labs"},
		}},
	}
	matchingLabels := map[string]bool{"agent-ready": true}
	tests := []struct {
		name        string
		team        string
		state       string
		projectName string
		labels      map[string]bool
		assignee    string
		want        bool
	}{
		{name: "all filters", team: "ADV", state: "Todo", projectName: "Adversary Labs", labels: matchingLabels, assignee: "marc", want: true},
		{name: "team mismatch", team: "OTHER", state: "Todo", projectName: "Adversary Labs", labels: matchingLabels, assignee: "marc"},
		{name: "state mismatch", team: "ADV", state: "Backlog", projectName: "Adversary Labs", labels: matchingLabels, assignee: "marc"},
		{name: "project mismatch", team: "ADV", state: "Todo", projectName: "Platform", labels: matchingLabels, assignee: "marc"},
		{name: "required label missing", team: "ADV", state: "Todo", projectName: "Adversary Labs", labels: map[string]bool{}, assignee: "marc"},
		{name: "excluded label present", team: "ADV", state: "Todo", projectName: "Adversary Labs", labels: map[string]bool{"agent-ready": true, "blocked": true}, assignee: "marc"},
		{name: "assignee mismatch", team: "ADV", state: "Todo", projectName: "Adversary Labs", labels: matchingLabels, assignee: "alex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := linearWorkflowMatchesIssue(workflow, test.team, test.state, "", test.projectName, test.labels, test.assignee)
			if got != test.want {
				t.Fatalf("linearWorkflowMatchesIssue() = %v, want %v", got, test.want)
			}
		})
	}
}
