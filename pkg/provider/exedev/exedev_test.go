package exedev

import (
	"path"
	"testing"
)

func TestExpandRemotePathLocalRules(t *testing.T) {
	t.Parallel()

	// Non-tilde paths must pass through unchanged (no remote call needed).
	p := &Provider{}
	for _, in := range []string{
		"/home/exedev/workspace/AGENTS.md",
		"/home/exedev/.openclaw/workspace/SOUL.md",
		"relative/path.md",
		"",
	} {
		got, err := p.expandRemotePath(t.Context(), "unused-vm", in)
		if err != nil {
			t.Fatalf("expandRemotePath(%q): %v", in, err)
		}
		if got != in {
			t.Fatalf("expandRemotePath(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestExpandRemotePathJoinShape(t *testing.T) {
	t.Parallel()
	// Document the absolute layout callers should use after resolving HOME.
	home := "/home/exedev"
	if got, want := path.Join(home, "workspace", "AGENTS.md"), "/home/exedev/workspace/AGENTS.md"; got != want {
		t.Fatalf("staged path = %q, want %q", got, want)
	}
	if got, want := path.Join(home, ".openclaw", "workspace", "AGENTS.md"), "/home/exedev/.openclaw/workspace/AGENTS.md"; got != want {
		t.Fatalf("live path = %q, want %q", got, want)
	}
	// Regression: shell-quoted "~/workspace" used to create this literal path.
	if got, want := path.Join(home, "~", "workspace", "AGENTS.md"), "/home/exedev/~/workspace/AGENTS.md"; got != want {
		t.Fatalf("literal-tilde path shape = %q, want %q", got, want)
	}
}
