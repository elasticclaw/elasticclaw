package types

import "testing"

func TestIsOpenClawAgentDBFile(t *testing.T) {
	for path, want := range map[string]bool{
		".openclaw/agents/main/agent/openclaw-agent.sqlite":                          true,
		".openclaw/agents/main/agent/openclaw-agent.sqlite-wal":                      true,
		".openclaw/agents/main/agent/openclaw-agent.sqlite-shm":                      true,
		".openclaw/agents/main/agent/openclaw-agent.sqlite.generation-writer.sqlite": true,
		".openclaw/agents/main/agent/openclaw-agent.sqlite.tmp":                      true,
		"/home/daytona/.openclaw/agents/main/agent/openclaw-agent.sqlite-wal":        true,
		"workspace/openclaw-agent.sqlite-backup":                                     false,
		"/home/daytona/.openclaw/workspace/openclaw-agent.sqlite":                    false,
		".openclaw/agents/main/agent/openclaw.sqlite":                                false,
		".openclaw/agents/main/agent/auth-profiles.json":                             false,
	} {
		if got := IsOpenClawAgentDBFile(path); got != want {
			t.Errorf("IsOpenClawAgentDBFile(%q) = %v, want %v", path, got, want)
		}
	}
}
