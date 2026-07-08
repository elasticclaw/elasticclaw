"use client"

import { useCallback, useEffect, useState } from "react"
import { fetchWorkspaces, type RepositoryAccess, type Workspace } from "@/lib/api"
import { YamlHighlight } from "./yaml-highlight"

export default function WorkspacesSection({ selectedWorkspace }: { selectedWorkspace: string }) {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setWorkspaces(await fetchWorkspaces())
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load workspaces")
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load() }, [load])

  const visibleWorkspaces = workspaces.filter((workspace) => workspace.name === selectedWorkspace)

  return (
    <div className="space-y-6">
      {error && (
        <div className="text-sm text-red-500 bg-red-500/10 border border-red-500/20 rounded-md px-3 py-2">
          {error}
        </div>
      )}

      {loading && visibleWorkspaces.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center animate-pulse">Loading workspace…</p>
      ) : workspaces.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No workspaces pushed yet.</p>
      ) : visibleWorkspaces.length === 0 ? (
        <p className="text-sm text-muted-foreground px-4 py-6 text-center">No workspace named {selectedWorkspace} is configured.</p>
      ) : (
        <div className="space-y-4">
          {visibleWorkspaces.map((workspace) => (
            <div key={workspace.name} className="space-y-4">
              <h2 className="text-base font-semibold">{workspace.name}</h2>
              <div className="overflow-hidden rounded-md border border-border bg-muted/40">
                <YamlHighlight code={workspace.config || "No elasticclaw-config.yaml content available."} />
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// Currently unreferenced (kept from the original settings-content.tsx).
export function WorkspaceRepositoryList({ values }: { values: RepositoryAccess[] }) {
  return (
    <div className="space-y-1">
      <h4 className="text-xs font-medium text-muted-foreground">Repositories</h4>
      {values.length === 0 ? (
        <p className="text-xs text-muted-foreground/70">None</p>
      ) : (
        <div className="space-y-1">
          {values.slice(0, 4).map((value) => (
            <div key={value.repo} className="flex items-center justify-between gap-2 text-xs bg-muted px-2 py-1 rounded">
              <code className="truncate">{value.repo}</code>
              <span className="shrink-0 text-muted-foreground">{value.permissions || "read"}</span>
            </div>
          ))}
          {values.length > 4 && (
            <p className="text-xs text-muted-foreground">+{values.length - 4} more</p>
          )}
        </div>
      )}
    </div>
  )
}

// Currently unreferenced (kept from the original settings-content.tsx).
export function WorkspaceAccessList({ title, values }: { title: string; values: string[] }) {
  return (
    <div className="space-y-1">
      <h4 className="text-xs font-medium text-muted-foreground">{title}</h4>
      {values.length === 0 ? (
        <p className="text-xs text-muted-foreground/70">None</p>
      ) : (
        <div className="space-y-1">
          {values.slice(0, 4).map((value) => (
            <code key={value} className="block text-xs bg-muted px-2 py-1 rounded truncate">
              {value}
            </code>
          ))}
          {values.length > 4 && (
            <p className="text-xs text-muted-foreground">+{values.length - 4} more</p>
          )}
        </div>
      )}
    </div>
  )
}
