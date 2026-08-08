package exedev

import (
	"path"
	"strings"
	"testing"
)

func TestShellQuoteJoinsArgsAsSingleRemoteCommand(t *testing.T) {
	t.Parallel()
	// Matches how pipeline_runner invokes Exec for workflow run actions.
	workspaceCmd := `~/.elasticclaw/flake-run bash -lc 'cd "$HOME/.openclaw/workspace" && bash scripts/verify-github-pr-links.sh'`
	got := shellQuote([]string{"bash", "-lc", workspaceCmd})
	// Must be one shell word-stream that re-expands to bash -lc <full script>.
	if !strings.HasPrefix(got, "'bash' '-lc' '") {
		t.Fatalf("shellQuote prefix = %q, want 'bash' '-lc' '…'", got)
	}
	if !strings.Contains(got, "verify-github-pr-links.sh") {
		t.Fatalf("shellQuote missing script path: %q", got)
	}
	// Inner single quotes must be escaped so the remote shell keeps the full -lc payload.
	if strings.Count(got, "bash") < 2 {
		t.Fatalf("shellQuote should embed nested bash -lc: %q", got)
	}
	// Regression: multi-arg ssh used to drop everything after the first word of
	// the -lc payload; a single remote argv is what Exec must pass to ssh.
	if strings.Contains(got, "\x00") {
		t.Fatal("shellQuote must not embed NULs")
	}
}

func TestShellQuoteEscapesEmbeddedSingleQuotes(t *testing.T) {
	t.Parallel()
	got := shellQuote([]string{"echo", "it's"})
	want := `'echo' 'it'"'"'s'`
	if got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}
}

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
