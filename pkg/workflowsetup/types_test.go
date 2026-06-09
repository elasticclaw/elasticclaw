package workflowsetup

import (
	"encoding/json"
	"testing"
)

func TestPatternMetadataJSONShape(t *testing.T) {
	metadata := PatternMetadata{
		ID:          "github-issues",
		Label:       "GitHub Issues",
		Description: "Create claws from labeled GitHub issues.",
		RequiredFields: []PatternField{
			{Path: "trigger.github_issues.repositories", Label: "Repositories", Description: "Repositories to watch."},
		},
		AdvancedFields: []PatternField{
			{Path: "concurrency_group", Label: "Concurrency group", Description: "Limit concurrent runs."},
		},
		Defaults: map[string]interface{}{
			"enabled": true,
		},
		ValidationFieldPaths: []string{"workflow.name", "trigger.github_issues.repositories"},
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal pattern metadata: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pattern metadata: %v", err)
	}

	for _, key := range []string{"id", "label", "description", "requiredFields", "advancedFields", "defaults", "validationFieldPaths"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("pattern metadata JSON missing key %q in %s", key, data)
		}
	}
}

func TestRenderContractsJSONShape(t *testing.T) {
	request := RenderRequest{
		WorkflowName: "issue-triage",
		PatternID:    "github-issues",
		Config: map[string]interface{}{
			"enabled": true,
		},
	}
	response := RenderResponse{
		WorkflowName: "issue-triage",
		Config:       "name: issue-triage\n",
		ConfigHash:   ConfigHash("name: issue-triage\n"),
		Warnings: []Diagnostic{
			{ID: "optional-field", Severity: SeverityWarning, OK: true},
		},
	}

	assertJSONKeys(t, "render request", request, []string{"workflowName", "patternId", "config"})
	assertJSONKeys(t, "render response", response, []string{"workflowName", "config", "configHash", "warnings"})
}

func TestRenderRequestPatternIDJSONRoundTrip(t *testing.T) {
	input := []byte(`{"workflowName":"issue-triage","patternId":"github-issues","config":{"enabled":true}}`)

	var request RenderRequest
	if err := json.Unmarshal(input, &request); err != nil {
		t.Fatalf("unmarshal render request: %v", err)
	}

	if got := request.PatternID; got != "github-issues" {
		t.Fatalf("render request PatternID = %q, want %q", got, "github-issues")
	}

	data, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal render request: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal marshaled render request: %v", err)
	}
	if got["patternId"] != "github-issues" {
		t.Fatalf("render request patternId JSON = %#v, want %q in %s", got["patternId"], "github-issues", data)
	}
}

func TestValidateContractsJSONShape(t *testing.T) {
	request := ValidateRequest{
		WorkflowName: "issue-triage",
		Config:       "name: issue-triage\n",
	}
	response := ValidateResponse{
		OK:         true,
		ConfigHash: ConfigHash(request.Config),
		Summary:    Summary{Warning: 1},
		Checks: []Diagnostic{
			{ID: "name", Severity: SeverityInfo, OK: true, Status: "passed"},
		},
	}

	assertJSONKeys(t, "validate request", request, []string{"workflowName", "config"})
	assertJSONKeys(t, "validate response", response, []string{"ok", "configHash", "summary", "checks"})
}

func TestSaveRequestJSONShape(t *testing.T) {
	request := SaveRequest{
		Workspace: "main",
		Workflow: SaveWorkflow{
			Name:   "issue-triage",
			Config: "name: issue-triage\n",
		},
		Mode:                SaveModeCreate,
		ValidatedConfigHash: ConfigHash("name: issue-triage\n"),
		AllowWarnings:       true,
	}

	data, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal save request: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal save request: %v", err)
	}
	for _, key := range []string{"workspace", "workflow", "mode", "validatedConfigHash", "allowWarnings"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("save request JSON missing key %q in %s", key, data)
		}
	}

	workflow, ok := got["workflow"].(map[string]interface{})
	if !ok {
		t.Fatalf("workflow JSON = %#v, want object", got["workflow"])
	}
	for _, key := range []string{"name", "config"} {
		if _, ok := workflow[key]; !ok {
			t.Fatalf("save request workflow JSON missing key %q in %s", key, data)
		}
	}
}

func assertJSONKeys(t *testing.T, label string, value interface{}, keys []string) {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", label, err)
	}

	for _, key := range keys {
		if _, ok := got[key]; !ok {
			t.Fatalf("%s JSON missing key %q in %s", label, key, data)
		}
	}
}
