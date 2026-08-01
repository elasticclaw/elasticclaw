package hub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/config"
)

// A push carrying one unsafe key must be rejected outright. Before this, the
// template directory was wiped before any key was validated, so a single bad
// key destroyed the stored template and left a silently truncated one behind
// while saveExternalTemplate still returned nil.
func TestSaveExternalTemplateRejectsInvalidPathBeforeWipe(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))

	good := map[string]string{
		"AGENTS.md":       "agents",
		"memory/notes.md": "note",
	}
	if err := saveExternalTemplate("faster", good); err != nil {
		t.Fatalf("saveExternalTemplate (initial): %v", err)
	}

	bad := map[string]string{
		"AGENTS.md":    "replaced",
		"../escape.md": "nope",
	}
	err := saveExternalTemplate("faster", bad)
	if err == nil {
		t.Fatal("saveExternalTemplate succeeded, want invalid path error")
	}
	if !strings.Contains(err.Error(), "../escape.md") {
		t.Errorf("error = %v, want it to name the offending key %q", err, "../escape.md")
	}

	dir := filepath.Join(templatesDir(), "faster")
	got, err := config.ReadTemplateFiles(dir)
	if err != nil {
		t.Fatalf("ReadTemplateFiles: %v", err)
	}
	for name, want := range good {
		if got[name] != want {
			t.Errorf("template file %s = %q, want %q (previous template must survive a rejected push)", name, got[name], want)
		}
	}
	if _, ok := got["escape.md"]; ok {
		t.Error("traversal path must not be written")
	}
}
