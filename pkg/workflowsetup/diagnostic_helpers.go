package workflowsetup

func criticalDiagnostic(id, category, fieldPath, title, detail string) Diagnostic {
	return Diagnostic{
		ID:        id,
		Category:  category,
		Severity:  SeverityCritical,
		OK:        false,
		Blocking:  true,
		Step:      "validate-static",
		FieldPath: fieldPath,
		Title:     title,
		Detail:    detail,
		FixTarget: fieldPath,
		FixLabel:  "Update config",
		Retryable: true,
		Status:    "failed",
	}
}

func validateResponse(configHashInput string, checks []Diagnostic) ValidateResponse {
	ok := true
	for _, check := range checks {
		if check.Blocking && check.Severity == SeverityCritical {
			ok = false
			break
		}
	}
	return ValidateResponse{
		OK:         ok,
		ConfigHash: ConfigHash(configHashInput),
		Summary:    SummarizeDiagnostics(checks),
		Checks:     checks,
	}
}
