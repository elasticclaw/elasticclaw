package docker

import (
	"context"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
)

func TestCopyInRejectsRelativeDestination(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	err = provider.CopyIn(context.Background(), "container", "relative/path.txt", []byte("content"))
	if err == nil {
		t.Fatal("expected relative destination to be rejected")
	}
}

func TestNewDefaultsToPinnedOpenClawImage(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	if got, want := provider.cfg.Image, "ghcr.io/openclaw/openclaw:"+cliversion.OpenClawVersion; got != want {
		t.Fatalf("default image = %q, want %q", got, want)
	}
}

func TestParentPath(t *testing.T) {
	tests := map[string]string{
		"/home/node/.elasticclaw/bin": "/home/node/.elasticclaw",
		"/home/node/workspace":        "/home/node",
		"/home":                       "/",
		"/":                           "",
		"":                            "",
	}
	for input, want := range tests {
		if got := parentPath(input); got != want {
			t.Fatalf("parentPath(%q) = %q, want %q", input, got, want)
		}
	}
}
