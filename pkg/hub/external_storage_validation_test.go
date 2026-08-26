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

// TestReservedWorkspaceNamesCoverFrontendSections reads the frontend's
// VALID_SECTIONS list and asserts every slug in it is reserved here and served
// as a static section. The reserved list ranges over itself in the test above,
// so a section added to sections.ts but forgotten here would otherwise go
// unnoticed until a workspace of that name lost its whole settings UI.
func TestReservedWorkspaceNamesCoverFrontendSections(t *testing.T) {
	path := filepath.Join("..", "..", "web", "app", "settings", "[[...parts]]", "sections.ts")
	source, err := os.ReadFile(path) //nolint:gosec // fixed path inside the repo
	if os.IsNotExist(err) {
		t.Skipf("%s not present in this checkout", path)
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	sections := frontendValidSections(t, string(source))
	if len(sections) == 0 {
		t.Fatalf("no sections parsed out of %s", path)
	}
	for _, section := range sections {
		if !reservedWorkspaceNames[section] {
			t.Errorf("section %q is missing from reservedWorkspaceNames", section)
		}
		if !settingsStaticSection(section) {
			t.Errorf("section %q is missing from settingsStaticSection", section)
		}
	}
}

// frontendValidSections pulls the quoted slugs out of the VALID_SECTIONS array
// literal in sections.ts.
func frontendValidSections(t *testing.T, source string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(source, "VALID_SECTIONS = [")
	if !ok {
		t.Fatal("VALID_SECTIONS array not found in sections.ts")
	}
	body, _, ok := strings.Cut(rest, "]")
	if !ok {
		t.Fatal("VALID_SECTIONS array is not terminated in sections.ts")
	}
	var sections []string
	for _, field := range strings.Split(body, ",") {
		if slug := strings.Trim(strings.TrimSpace(field), `"`); slug != "" {
			sections = append(sections, slug)
		}
	}
	return sections
}
