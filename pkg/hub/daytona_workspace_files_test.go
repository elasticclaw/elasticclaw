package hub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/config"
)

// The write loop and the readiness gate both go through daytonaWorkspaceFilePath,
// so pinning it here is what stops a regression back to the staged
// /home/daytona/workspace dir that nothing syncs on Daytona.
func TestDaytonaWorkspaceFilePathTargetsLiveWorkspace(t *testing.T) {
	cases := map[string]string{
		"AGENTS.md":                             "/home/daytona/.openclaw/workspace/AGENTS.md",
		"scripts/detect_android_changes.py":     "/home/daytona/.openclaw/workspace/scripts/detect_android_changes.py",
		"scripts/review-loop/reviewers/arch.md": "/home/daytona/.openclaw/workspace/scripts/review-loop/reviewers/arch.md",
	}
	for name, want := range cases {
		if got := daytonaWorkspaceFilePath(name); got != want {
			t.Errorf("daytonaWorkspaceFilePath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDaytonaWorkspaceFilesReadinessCommandChecksNestedPaths(t *testing.T) {
	cmd := daytonaWorkspaceFilesReadinessCommand(map[string]string{
		"AGENTS.md":                             "a",
		"scripts/detect_android_changes.py":     "b",
		"scripts/review-loop/reviewers/arch.md": "c",
	})

	for _, want := range []string{
		"/home/daytona/.openclaw/workspace/AGENTS.md",
		"/home/daytona/.openclaw/workspace/scripts/detect_android_changes.py",
		"/home/daytona/.openclaw/workspace/scripts/review-loop/reviewers/arch.md",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("readiness command missing check for %s:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "/home/daytona/workspace/") {
		t.Errorf("readiness command must verify the live workspace, not the staged one:\n%s", cmd)
	}
	if got := daytonaWorkspaceFilesReadinessCommand(nil); got != "true" {
		t.Errorf("empty file set = %q, want \"true\"", got)
	}
}

func TestDaytonaExecutableWorkspaceFile(t *testing.T) {
	cases := map[string]bool{
		"scripts/run.sh":                     true,
		"scripts/detect_android_changes.py":  true,
		"AGENTS.md":                          false,
		"scripts/review-loop/REVIEW_LOOP.md": false,
	}
	for name, want := range cases {
		if got := daytonaExecutableWorkspaceFile(name); got != want {
			t.Errorf("daytonaExecutableWorkspaceFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// A workspace with nested subdirectories must round-trip through template
// storage with its subtree intact: before this, saveExternalTemplate dropped
// every path containing a slash, silently losing scripts/ and memory/.
func TestSaveExternalTemplatePreservesNestedFiles(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))

	files := map[string]string{
		"AGENTS.md":                             "agents",
		"scripts/detect_android_changes.py":     "print('hi')",
		"scripts/review-loop/reviewers/arch.md": "reviewer",
		"memory/notes.md":                       "note",
	}
	if err := saveExternalTemplate("faster", files); err != nil {
		t.Fatalf("saveExternalTemplate: %v", err)
	}

	dir := filepath.Join(templatesDir(), "faster")
	got, err := config.ReadTemplateFiles(dir)
	if err != nil {
		t.Fatalf("ReadTemplateFiles: %v", err)
	}
	for _, name := range []string{
		"AGENTS.md",
		"scripts/detect_android_changes.py",
		"scripts/review-loop/reviewers/arch.md",
		"memory/notes.md",
	} {
		if _, ok := got[name]; !ok {
			t.Errorf("template files missing %s (got %v)", name, sortedWorkspaceFileNames(got))
		}
	}
}
