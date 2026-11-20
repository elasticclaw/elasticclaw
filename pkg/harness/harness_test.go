package harness

import "testing"

func TestWorkspacePath(t *testing.T) {
	const want = "/home/daytona/.openclaw/workspace"
	if got := WorkspacePath("/home/daytona"); got != want {
		t.Fatalf("WorkspacePath() = %q, want %q", got, want)
	}
}
