//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDockerWorkflowE2E(t *testing.T) {
	runID := e2eRunID()
	env := newE2EEnv(t, runID, "docker")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	workspaceName := "e2e-docker-" + env.RunID
	workflowName := "docker-" + env.RunID
	agentName := "docker-smoke-" + env.RunID

	cleanupProvider(ctx, t, env)
	hub := startHub(ctx, t, env)
	root := writeDockerWorkspaceFixture(t, env, workspaceName, workflowName)

	var agentID string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		var provider, providerID string
		if agentID != "" {
			provider, providerID = hub.agentProvider(cleanupCtx, t, agentID)
			_ = hub.deleteAgent(cleanupCtx, agentID)
		}
		if providerID != "" {
			destroyProviderInstanceByID(cleanupCtx, t, env, provider, providerID)
		}
	})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		cleanupProvider(cleanupCtx, t, env)
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	runCLI(ctx, t, root, env, "workspace", "push", workspaceName)
	runCLI(ctx, t, root, env, "workflow", "push", "--workspace", workspaceName, filepath.Join(root, ".elasticclaw", "workflows", "docker.yaml"))
	runCLI(ctx, t, root, env, "workflow", "trigger", workflowName, "--workspace", workspaceName, "--input", "task="+agentName)

	agentID = waitForOneAgent(ctx, t, hub, agentName)
	waitForAgentStatus(ctx, t, hub, agentID, "connected")
}

func writeDockerWorkspaceFixture(t *testing.T, env e2eEnv, workspaceName, workflowName string) string {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, ".elasticclaw", "workspaces", workspaceName)
	workflowDir := filepath.Join(root, ".elasticclaw", "workflows")
	if err := os.MkdirAll(workspaceDir, 0750); err != nil {
		t.Fatalf("mkdir Docker workspace fixture: %v", err)
	}
	if err := os.MkdirAll(workflowDir, 0750); err != nil {
		t.Fatalf("mkdir Docker workflow fixture: %v", err)
	}
	writeFile(t, filepath.Join(workspaceDir, "elasticclaw-config.yaml"), fmt.Sprintf(`schema_version: v1
name: %s
provider: docker
`, workspaceName))
	writeFile(t, filepath.Join(workspaceDir, "AGENTS.md"), "You are an ElasticClaw Docker E2E agent. Keep responses concise.\n")
	writeFile(t, filepath.Join(workspaceDir, "TOOLS.md"), "Use tools only when needed.\n")
	writeFile(t, filepath.Join(workspaceDir, "CONTEXT.md"), "This is an ElasticClaw Docker workflow connectivity test.\n")
	writeFile(t, filepath.Join(workflowDir, "docker.yaml"), fmt.Sprintf(`schema_version: v1
name: %s
provider: docker
enable_manual_trigger: true

inputs:
  - name: task
    type: string
    required: true

concurrency_group: e2e-docker-%s

stages:
  - id: working
    label: Working
    entry: true
    on_enter:
      inject: |
        Confirm that the Docker workflow agent connected to the hub.
        Do not create a pull request.
`, workflowName, env.RunID))
	return root
}
