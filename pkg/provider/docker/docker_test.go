package docker

import (
	"context"
	"testing"
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

func TestNewDefaultsToElasticClawAgentImage(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	if got, want := provider.cfg.Image, "elasticclaw/claw-agent:dev"; got != want {
		t.Fatalf("default image = %q, want %q", got, want)
	}
}
