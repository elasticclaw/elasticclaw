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
