"use client"

import { useCallback, useEffect, useState } from "react"
import { Switch } from "@/components/ui/switch"
import { cn } from "@/lib/utils"
import { fetchWorkspaces, updateWorkflowControls, type Workspace, type Workflow } from "@/lib/api"

export default function WorkflowsSection({ selectedWorkspace }: { selectedWorkspace: string }) {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const [savingWorkflow, setSavingWorkflow] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setWorkspaces(await fetchWorkspaces())
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load workflows")
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const workflows = workspaces.flatMap((workspace) =>
    (workspace.workflows || []).map((workflow) => ({ ...workflow, workspaceName: workflow.workspaceName || workspace.name }))
  ).filter((workflow) => workflow.workspaceName === selectedWorkspace)

  const patchWorkflow = useCallback(async (workflow: Workflow, patch: { enabled?: boolean; enableManualTrigger?: boolean }) => {
    const key = `${workflow.workspaceName}/${workflow.name}`
    setSavingWorkflow(key)
    setError("")
    try {
      const updated = await updateWorkflowControls(workflow, patch)
      setWorkspaces(current => current.map(workspace => {
        if (workspace.name !== updated.workspaceName) return workspace
        return {
          ...workspace,
          workflows: (workspace.workflows || []).map(item =>
            item.name === updated.name ? { ...item, ...updated } : item
          ),
        }
      }))
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to update workflow")
    } finally {
      setSavingWorkflow("")
    }
  }, [])

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold mb-1">Workflows</h2>
        <p className="text-sm text-muted-foreground">
          Workflows define triggers and runtime behavior within this workspace.
        </p>
      </div>

      {error && (
        <div className="text-sm text-red-500 bg-red-500/10 border border-red-500/20 rounded-md px-3 py-2">
          {error}
        </div>
      )}

      {loading && workflows.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading workflows…</p>
      ) : workflows.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No workflows configured.</p>
      ) : (
        <div className="border border-border rounded-lg divide-y divide-border">
          {workflows.map((workflow) => (
            <WorkflowSummaryRow
              key={`${workflow.workspaceName}/${workflow.name}`}
              workflow={workflow}
              saving={savingWorkflow === `${workflow.workspaceName}/${workflow.name}`}
              onPatch={patchWorkflow}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function WorkflowSummaryRow({
  workflow,
  saving,
  onPatch,
}: {
  workflow: Workflow
  saving: boolean
  onPatch: (workflow: Workflow, patch: { enabled?: boolean; enableManualTrigger?: boolean }) => void
}) {
  return (
    <div className="px-4 py-3">
      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div className="min-w-0">
          <p className="text-sm font-medium truncate">{workflow.name}</p>
          <p className="text-xs text-muted-foreground truncate">
            {workflow.workspaceName} · {workflow.integration || "manual"}
            {workflow.triggerStatus ? ` · ${workflow.triggerStatus}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 shrink-0">
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <Switch
              checked={workflow.enabled}
              disabled={saving}
              onCheckedChange={(checked) => onPatch(workflow, { enabled: checked })}
              aria-label={`Toggle automatic handling for ${workflow.name}`}
            />
            <span>Auto handling</span>
          </label>
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <Switch
              checked={Boolean(workflow.enableManualTrigger)}
              disabled={saving}
              onCheckedChange={(checked) => onPatch(workflow, { enableManualTrigger: checked })}
              aria-label={`Toggle manual trigger for ${workflow.name}`}
            />
            <span>Manual trigger</span>
          </label>
          <span className={cn(
            "text-xs px-2 py-0.5 rounded",
            workflow.enabled ? "bg-muted text-muted-foreground" : "bg-amber-500/10 text-amber-500"
          )}>
            {workflow.enabled ? "enabled" : "paused"}
          </span>
        </div>
      </div>
    </div>
  )
}
