package v2_test

import (
	"strings"
	"testing"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestWorkflowAnalyticsAndTerminalOutcomes(t *testing.T) {
	for _, outcome := range []string{"delivered", "no_changes", "closed", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			doc := strings.Replace(validWorkflowYAML, "terminal: true", "terminal: true\n    outcome: "+outcome, 1)
			doc += "\nanalytics: {run_kind: maintenance, requires_pr: false, enabled: false}\n"
			resolved, err := v2.ParseAndValidateWorkflow([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			a := resolved.Workflow.Analytics
			if a.RunKind != "maintenance" || a.RequiresPR == nil || *a.RequiresPR || a.Enabled == nil || *a.Enabled {
				t.Fatalf("analytics overrides lost: %+v", a)
			}
			if string(resolved.Workflow.States["completed"].Outcome) != outcome {
				t.Fatal("outcome lost")
			}
		})
	}
	for name, doc := range map[string]string{
		"unknown outcome":       strings.Replace(validWorkflowYAML, "terminal: true", "terminal: true\n    outcome: succeeded", 1),
		"nonterminal outcome":   strings.Replace(validWorkflowYAML, "phase: build", "phase: build\n    outcome: delivered", 1),
		"unknown analytics key": validWorkflowYAML + "\nanalytics: {enable: true}\n",
		"unknown run kind":      validWorkflowYAML + "\nanalytics: {run_kind: pr_task}\n",
		"invalid bool":          validWorkflowYAML + "\nanalytics: {requires_pr: sometimes}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v2.ParseAndValidateWorkflow([]byte(doc)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
