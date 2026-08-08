package hub

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestPollAllPRsLogsWhenTokenResolutionFails(t *testing.T) {
	// Isolate from any developer workspace GitHub Apps under ~/.elasticclaw.
	tmp := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(tmp, "hub.yaml"))
	_ = os.MkdirAll(filepath.Join(tmp, "workspaces"), 0o750)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	insertWatcherTestPR(t, db, "claw-token", "pr-token")
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)
	s.pollAllPRs()
	// Empty hub + empty workspaces: pollAllPRs logs that no apps are configured
	// (not the per-PR "token resolution failed for all" path).
	got := buf.String()
	if !strings.Contains(got, "CRITICAL:") || !strings.Contains(got, "no GitHub Apps configured") {
		t.Fatalf("missing critical no-apps log: %s", got)
	}
}
