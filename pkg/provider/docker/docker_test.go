package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/types"
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

func TestDockerCreateArgsPublishesConfiguredPreviewPorts(t *testing.T) {
	args := dockerCreateArgs(
		Config{Image: "agent:test", Network: "elasticclaw"},
		types.CreateRequest{
			Name:         "ec-preview",
			PreviewPorts: []int{3000, 8080},
		},
	)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--network elasticclaw",
		"--publish 127.0.0.1::3000",
		"--publish 127.0.0.1::8080",
		"agent:test",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args missing %q: %s", want, joined)
		}
	}
}

func TestNewDefaultsToPinnedOpenClawImage(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	if got, want := provider.cfg.Image, cliversion.OpenClawImage; got != want {
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
