package workflowsetup

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSeverityValues(t *testing.T) {
	tests := map[Severity]string{
		SeverityCritical: "critical",
		SeverityWarning:  "warning",
		SeverityInfo:     "info",
	}

	for got, want := range tests {
		if string(got) != want {
			t.Fatalf("severity value = %q, want %q", got, want)
		}
	}
}

func TestDiagnosticJSONFields(t *testing.T) {
	diagnostic := Diagnostic{
		ID:        "workflow-name-required",
		Category:  "workflow",
		Severity:  SeverityCritical,
		OK:        false,
		Blocking:  true,
		Step:      "validate",
		FieldPath: "workflow.name",
		Title:     "Workflow name is required",
		Detail:    "Set a workflow name before saving.",
		FixTarget: "workflow.name",
		FixLabel:  "Add name",
		Retryable: true,
		Status:    "failed",
	}

	data, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatalf("marshal diagnostic: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal diagnostic: %v", err)
	}

	wantKeys := []string{
		"id",
		"category",
		"severity",
		"ok",
		"blocking",
		"step",
		"fieldPath",
		"title",
		"detail",
		"fixTarget",
		"fixLabel",
		"retryable",
		"status",
	}
	for _, key := range wantKeys {
		if _, ok := got[key]; !ok {
			t.Fatalf("diagnostic JSON missing key %q in %s", key, data)
		}
	}
	for _, key := range []string{"FieldPath", "FixTarget", "FixLabel"} {
		if _, ok := got[key]; ok {
			t.Fatalf("diagnostic JSON used Go field name %q in %s", key, data)
		}
	}
	if got["severity"] != "critical" {
		t.Fatalf("severity JSON = %q, want critical", got["severity"])
	}
}

func TestDiagnosticHasNoSecretValueFields(t *testing.T) {
	typ := reflect.TypeOf(Diagnostic{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		names := []string{strings.ToLower(field.Name), strings.ToLower(jsonName)}
		for _, name := range names {
			if strings.Contains(name, "secret") ||
				strings.Contains(name, "token") ||
				strings.Contains(name, "credential") ||
				strings.Contains(name, "apikey") ||
				name == "value" {
				t.Fatalf("diagnostic field %q exposes secret-like data", field.Name)
			}
		}
	}
}

func TestSummarizeDiagnosticsCountsSeverities(t *testing.T) {
	summary := SummarizeDiagnostics([]Diagnostic{
		{Severity: SeverityCritical},
		{Severity: SeverityWarning},
		{Severity: SeverityWarning},
		{Severity: SeverityInfo},
	})

	if summary.Critical != 1 {
		t.Fatalf("critical count = %d, want 1", summary.Critical)
	}
	if summary.Warning != 2 {
		t.Fatalf("warning count = %d, want 2", summary.Warning)
	}
	if summary.Info != 1 {
		t.Fatalf("info count = %d, want 1", summary.Info)
	}
}
