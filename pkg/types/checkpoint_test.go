package types

import "testing"

func TestIsOpenClawAgentDBFile(t *testing.T) {
	for path, want := range map[string]bool{
		"openclaw-agent.sqlite":                                 true,
		"openclaw-agent.sqlite-wal":                             true,
		"openclaw-agent.sqlite-shm":                             true,
		"openclaw-agent.sqlite-journal":                         true,
		"openclaw-agent.sqlite.generation-writer.sqlite":        true,
		"openclaw-agent.sqlite.reindex-lock.sqlite-wal":         true,
		".openclaw/agents/main/agent/openclaw-agent.sqlite-wal": true,
		"openclaw-agent.sqlite-backup":                          false,
		"openclaw-agent.sqlite3":                                false,
		"workspace/notes/openclaw-agent.sqlite.bak":             false,
		"openclaw.sqlite":                                       false,
	} {
		if got := IsOpenClawAgentDBFile(path); got != want {
			t.Errorf("IsOpenClawAgentDBFile(%q) = %v, want %v", path, got, want)
		}
	}
}
