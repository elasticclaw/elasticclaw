package checkpoints

import (
	"strings"
	"testing"
)

// Moved from pkg/hub/checkpoints_test.go with the extraction: the test only
// exercises the pure shell-quoting helpers, so it lives with them. The
// remaining checkpoint tests stay in pkg/hub because they drive hub
// internals (hand-built Server instances and clawConn state).

func TestDaytonaRestoreCommandSingleQuotesPaths(t *testing.T) {
	remote := "/home/daytona/.openclaw/workspace/src/$SECRET/`touch /tmp/pwn`/it's.txt"
	cmd := checkpointDaytonaRestoreCommand(remote, []byte("hello"))

	if !strings.Contains(cmd, "mkdir -p "+checkpointShellQuote("/home/daytona/.openclaw/workspace/src/$SECRET/`touch /tmp/pwn`")) {
		t.Fatalf("expected single-quoted mkdir path, got %q", cmd)
	}
	if !strings.Contains(cmd, "> "+checkpointShellQuote(remote)) {
		t.Fatalf("expected single-quoted output path, got %q", cmd)
	}
	if strings.Contains(cmd, `mkdir -p "`) || strings.Contains(cmd, `> "`) {
		t.Fatalf("expected restore paths to avoid shell double quotes, got %q", cmd)
	}
}
