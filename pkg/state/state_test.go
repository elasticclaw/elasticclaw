package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestStoreRejectsEscapingInstanceNames(t *testing.T) {
	store := &Store{basePath: t.TempDir()}
	victim := filepath.Join(filepath.Dir(store.basePath), "victim")
	if err := os.MkdirAll(victim, 0755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}

	for _, name := range []string{"../victim", "a/b", "..", filepath.Join(store.basePath, "abs")} {
		if err := store.Save(&types.Instance{Name: name}); err == nil {
			t.Fatalf("Save(%q) succeeded, want error", name)
		}
		if _, err := store.Get(name); err == nil {
			t.Fatalf("Get(%q) succeeded, want error", name)
		}
		if err := store.Delete(name); err == nil {
			t.Fatalf("Delete(%q) succeeded, want error", name)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim path was modified: %v", err)
	}
}
