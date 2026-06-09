package workflowsetup

// Severity classifies diagnostic impact for workflow setup checks.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Diagnostic is safe to return to clients. It identifies the failing field and
// suggested action, but it intentionally has no secret or raw value fields.
type Diagnostic struct {
	ID        string   `json:"id"`
	Category  string   `json:"category"`
	Severity  Severity `json:"severity"`
	OK        bool     `json:"ok"`
	Blocking  bool     `json:"blocking"`
	Step      string   `json:"step"`
	FieldPath string   `json:"fieldPath"`
	Title     string   `json:"title"`
	Detail    string   `json:"detail"`
	FixTarget string   `json:"fixTarget"`
	FixLabel  string   `json:"fixLabel"`
	Retryable bool     `json:"retryable"`
	Status    string   `json:"status"`
}

// Summary counts diagnostics by severity.
type Summary struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

// SummarizeDiagnostics builds a severity summary from diagnostics.
func SummarizeDiagnostics(checks []Diagnostic) Summary {
	var summary Summary
	for _, check := range checks {
		switch check.Severity {
		case SeverityCritical:
			summary.Critical++
		case SeverityWarning:
			summary.Warning++
		case SeverityInfo:
			summary.Info++
		}
	}
	return summary
}
