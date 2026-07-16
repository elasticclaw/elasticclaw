package hub

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestPollAllPRsLogsWhenTokenResolutionFails(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	insertWatcherTestPR(t, db, "claw-token", "pr-token")
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)
	s.pollAllPRs()
	if !strings.Contains(buf.String(), "CRITICAL: GitHub token resolution failed") {
		t.Fatalf("missing critical log: %s", buf.String())
	}
}
