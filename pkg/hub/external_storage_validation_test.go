package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSaveExternalFactoryIsRetired(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	err := saveExternalFactory(&types.FactoryConfig{
		Name:          "retired-factory",
		Integration:   "linear",
		TriggerStatus: "Ready",
		Template:      "elasticclaw",
	})
	if err == nil {
		t.Fatal("saveExternalFactory succeeded, want retired error")
	}
	if !strings.Contains(err.Error(), "factories are retired") {
		t.Fatalf("error = %v, want factories are retired", err)
	}
}

func TestLoadExternalFactoriesIgnoresDisk(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	// Even if a leftover factories/ tree exists, load must return empty.
	dir := filepath.Join(legacyFactoriesDir(), "ghost")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "factory.yaml"), []byte("name: ghost\nintegration: linear\ntrigger_status: Todo\ntemplate: elasticclaw\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	factories, err := loadExternalFactories()
	if err != nil {
		t.Fatalf("loadExternalFactories: %v", err)
	}
	if len(factories) != 0 {
		t.Fatalf("loadExternalFactories returned %d factories, want 0", len(factories))
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
