// Package harness holds what ElasticClaw needs to know about the agent runtime it runs in a sandbox; OpenClaw is the only runtime today.
package harness

import "path/filepath"

// WorkspaceDir is the agent's live workspace, relative to the sandbox user's home.
// Customer prompts and templates hardcode ~/.openclaw/workspace, so this path
// must keep resolving even if a future harness stores its workspace elsewhere.
const WorkspaceDir = ".openclaw/workspace"

// WorkspacePath returns WorkspaceDir under home.
func WorkspacePath(home string) string { return filepath.Join(home, WorkspaceDir) }
