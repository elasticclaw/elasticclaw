package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestInstanceIDTraversalRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stateOutside := filepath.Join(home, ".elasticclaw", "state", "outside")
	if err := os.MkdirAll(stateOutside, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	p := New()
	if err := p.Destroy(context.Background(), "../outside", false); err == nil {
		t.Fatal("Destroy should reject traversal instance ID")
	}
	if _, err := os.Stat(stateOutside); err != nil {
		t.Fatalf("Destroy removed or changed escaped path: %v", err)
	}

	if _, err := p.Exec(context.Background(), "../outside", []string{"true"}); err == nil {
		t.Fatal("Exec should reject traversal instance ID")
	}

	absID := filepath.Join(home, "absolute")
	if _, err := p.Status(context.Background(), absID); err == nil {
		t.Fatal("Status should reject absolute instance ID")
	}
}

func TestCreateRejectsEscapingTemplatePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	p := New()
	_, err := p.Create(context.Background(), types.CreateRequest{
		Name: "safe",
		TemplateFiles: map[string][]byte{
			"../outside.txt": []byte("escaped"),
		},
	})
	if err == nil {
		t.Fatal("Create should reject traversal template path")
	}

	escapedPath := filepath.Join(home, ".elasticclaw", "state", "local-instances", "safe", "outside.txt")
	if _, err := os.Stat(escapedPath); !os.IsNotExist(err) {
		t.Fatalf("Create wrote escaped template path, stat err: %v", err)
	}

	absPath := filepath.Join(home, "absolute.txt")
	_, err = p.Create(context.Background(), types.CreateRequest{
		Name: "safe-absolute",
		TemplateFiles: map[string][]byte{
			absPath: []byte("escaped"),
		},
	})
	if err == nil {
		t.Fatal("Create should reject absolute template path")
	}
	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Fatalf("Create wrote absolute template path, stat err: %v", err)
	}
}
