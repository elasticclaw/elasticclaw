package docker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestCreateUsesDigestQualifiedOpenClawImage(t *testing.T) {
	for _, image := range []string{"", cliversion.OpenClawImage} {
		t.Run(image, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args")
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_TEST_ARGS\"\nprintf 'container-id\\n'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCKER_TEST_ARGS", argsFile)
			provider, err := New(Config{Image: image})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Create(context.Background(), types.CreateRequest{Name: "test"}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			want := []string{"run", "-d", "--name", "test", "--label", "elasticclaw.claw=test", "--add-host", "host.docker.internal:host-gateway", "--entrypoint", "sh", cliversion.OpenClawImage, "-lc", "trap 'exit 0' TERM INT; while :; do sleep 3600; done"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("docker args = %q, want %q", got, want)
			}
		})
	}
}
