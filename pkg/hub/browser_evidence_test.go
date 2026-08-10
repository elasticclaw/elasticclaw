package hub

import (
	"strings"
	"testing"
)

func TestWorkspaceTemplateFilesAddsBrowserEvidenceTools(t *testing.T) {
	original := map[string]string{"TOOLS.md": "# Tools\n", "AGENTS.md": "agents"}
	got := workspaceTemplateFiles(original)

	for _, want := range []string{
		"## Browser verification and PR evidence",
		"openclaw browser doctor",
		"browser-use doctor",
		"record start",
		"existing Playwright E2E runner",
		".artifacts/browser-evidence/branches/<safe-branch>",
		"manifest.json",
		".github/pr-evidence/<safe-branch>/<run>/",
		"short GIF preview",
		"H.264 MP4",
		"ffprobe",
		"entire evidence lifecycle must be autonomous",
		"Never require a person",
		"no documented attachment-upload API",
		"token video of an unchanged page",
		"Never add an evidence-only route",
		"Do not inject visual cursor markers",
		"must not alter its rendered product state",
		"fixture, session, entry point, and dev persona",
		"persisted backend state",
		"temporarily reverted or stashed",
		"full PR lifecycle",
		"private repositories",
		"Never commit credentials",
	} {
		if !strings.Contains(got["TOOLS.md"], want) {
			t.Errorf("TOOLS.md missing %q:\n%s", want, got["TOOLS.md"])
		}
	}
	if original["TOOLS.md"] != "# Tools\n" {
		t.Fatalf("workspaceTemplateFiles mutated input: %q", original["TOOLS.md"])
	}
}

func TestWorkspaceTemplateFilesAddsBrowserEvidenceToolsOnce(t *testing.T) {
	files := map[string]string{"TOOLS.md": browserEvidenceToolsSection}
	got := workspaceTemplateFiles(files)
	if count := strings.Count(got["TOOLS.md"], "## Browser verification and PR evidence"); count != 1 {
		t.Fatalf("browser evidence section count = %d, want 1", count)
	}
}

func TestBrowserEvidencePRPolicyRequiresTruthfulReviewerAccessibleEvidence(t *testing.T) {
	for _, want := range []string{
		"browser-visible changes",
		"Playwright-backed OpenClaw browser or Browser Use",
		"doctor gate",
		"final screenshot",
		"video/trace evidence",
		"console errors",
		"branch/run-scoped manifest",
		".github/pr-evidence/<safe-branch>/<run>/",
		"generate and commit a short GIF preview from that MP4",
		"Normalize the recording to H.264 MP4",
		"validate it with ffprobe",
		"never require manual PR edits",
		"no documented attachment-upload API",
		"observable before/action/after transition",
		"Never add or inject evidence-only routes",
		"must not alter its rendered product state",
		"fixture/session/persona",
		"counterfactually verify focused regression tests",
		"Preserve successful evidence",
		"never fabricate evidence",
	} {
		if !strings.Contains(defaultFactoryPRPolicy, want) {
			t.Errorf("defaultFactoryPRPolicy missing %q:\n%s", want, defaultFactoryPRPolicy)
		}
	}
}
