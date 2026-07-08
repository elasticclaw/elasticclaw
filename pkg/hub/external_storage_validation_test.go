package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSaveExternalFactoryRejectsInvalidExcludeLabels(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	err := saveExternalFactory(&types.FactoryConfig{
		Name:          "invalid-label-filter",
		Integration:   "linear",
		TriggerStatus: "Ready",
		Template:      "elasticclaw",
		ExcludeLabels: []string{" "},
	})
	if err == nil {
		t.Fatal("saveExternalFactory succeeded, want validation error")
	}
	if !strings.Contains(err.Error(), "exclude_labels[0] cannot be blank") {
		t.Fatalf("error = %v, want exclude_labels validation", err)
	}
}

func TestSaveExternalWorkspaceRejectsReservedNames(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	for name := range reservedWorkspaceNames {
		err := saveExternalWorkspace(&types.WorkspaceConfig{
			Name:  name,
			Files: map[string]string{"README.md": "test"},
		})
		if err == nil {
			t.Fatalf("saveExternalWorkspace(%q) succeeded, want reserved name error", name)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("saveExternalWorkspace(%q) error = %v, want reserved name error", name, err)
		}
	}

	// Non-reserved names should still work.
	err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name:  "engineering",
		Files: map[string]string{"README.md": "test"},
	})
	if err != nil {
		t.Fatalf("saveExternalWorkspace(%q) failed: %v", "engineering", err)
	}
}
