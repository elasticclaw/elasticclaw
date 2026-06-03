package hub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectToolLoop_CatchesElevatedFailures(t *testing.T) {
	content := `
exec failed: elevated is not available for this runtime
exec failed: elevated is not available for this runtime
exec failed: elevated is not available for this runtime
`
	if !detectToolLoop(content) {
		t.Fatal("expected detectToolLoop to catch repeated elevated failures")
	}
}

func TestDetectToolLoop_CatchesToolPolicyFailures(t *testing.T) {
	content := `
tool-policy: exec is not allowed
tool-policy: exec is not allowed
tool-policy: exec is not allowed
`
	if !detectToolLoop(content) {
		t.Fatal("expected detectToolLoop to catch repeated tool-policy failures")
	}
}

func TestDetectToolLoop_DoesNotTriggerOnSingleElevatedFailure(t *testing.T) {
	content := `
exec failed: elevated is not available for this runtime
some other output
`
	if detectToolLoop(content) {
		t.Fatal("expected detectToolLoop to NOT trigger on single elevated failure")
	}
}

func TestDetectToolLoop_StillCatchesEditFailures(t *testing.T) {
	content := `
edit failed: file not found
edit failed: file not found
edit failed: file not found
`
	if !detectToolLoop(content) {
		t.Fatal("expected detectToolLoop to still catch repeated edit failures")
	}
}

func TestDetectToolLoop_CatchesMixedToolFailures(t *testing.T) {
	// Mix of different tool failure patterns should still trigger
	content := `
edit failed: permission denied
exec failed: elevated is not available
tool-policy: write is not allowed
`
	// Each pattern appears only once, so no single pattern hits 3+
	if detectToolLoop(content) {
		t.Fatal("expected detectToolLoop to NOT trigger when each pattern appears only once")
	}

	// But 3 of one pattern should trigger
	content2 := `
exec failed: elevated is not available for this runtime
exec failed: elevated is not available for this runtime
exec failed: elevated is not available for this runtime
`
	if !detectToolLoop(content2) {
		t.Fatal("expected detectToolLoop to catch 3+ of same elevated pattern")
	}
}

func TestSanitizeBootstrapOutput_TruncatesWorkspaceDiagnostic(t *testing.T) {
	// Simulate a long workspace verification failure output
	longOutput := strings.Repeat("[daytona] verifying repo-a\n[daytona] verify FAILED: repo-a/.git missing\n", 20)
	got := sanitizeBootstrapOutput(longOutput)
	// Should be truncated to last 1200 chars
	if len(got) > 1200 {
		t.Fatalf("expected sanitized output to be truncated to <=1200 chars, got %d", len(got))
	}
	if !strings.Contains(got, "verify FAILED") {
		t.Fatal("expected sanitized output to contain the failure reason")
	}
}

func TestDaytonaRepoReadinessSnippetQuotesRepoName(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	repo := "owner/repo$(touch " + marker + ")"
	repoName := repoDirectoryName(repo)

	script := "cd " + shellQuote(dir) + "; mkdir -p " + shellQuote(repoName+"/.git") + "; " + daytonaRepoReadinessSnippet(repo)
	cmd := exec.Command("bash", "-c", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("readiness snippet failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repo name was evaluated as shell code; marker stat err=%v", err)
	}
}
