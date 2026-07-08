"use client"

import { useCallback, useEffect, useState } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import type { WorkspaceGitHubAppView } from "./types"

/**
 * Loads and mutates the GitHub Apps registered on a workspace
 * (/api/workspaces/{ws}/github-apps).
 */
export function useWorkspaceGitHubApps(workspace: string) {
  const [workspaceApps, setWorkspaceApps] = useState<WorkspaceGitHubAppView[]>([])
  const [workspaceLoading, setWorkspaceLoading] = useState(true)
  const [workspaceError, setWorkspaceError] = useState("")
  const hubUrl = getHubUrl()
  const token = () => getAuthToken() || ""

  const workspaceGitHubPath = workspace ? `/api/workspaces/${encodeURIComponent(workspace)}/github-apps` : ""
  const loadWorkspaceApps = useCallback(async () => {
    if (!workspace) return
    setWorkspaceLoading(true)
    setWorkspaceError("")
    try {
      const res = await fetch(`${hubUrl}${workspaceGitHubPath}`, { headers: { Authorization: `Bearer ${token()}` } })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setWorkspaceApps(data.githubApps || [])
    } catch (e) {
      setWorkspaceError(e instanceof Error ? e.message : "Failed to load GitHub Apps")
    } finally {
      setWorkspaceLoading(false)
    }
  }, [hubUrl, workspace, workspaceGitHubPath])

  useEffect(() => {
    loadWorkspaceApps()
  }, [loadWorkspaceApps])

  async function saveWorkspaceApp(body: { name: string; appId: number; url: string; installation: string; privateKeyPem: string }): Promise<boolean> {
    setWorkspaceError("")
    const res = await fetch(`${hubUrl}${workspaceGitHubPath}`, {
      method: "PUT",
      headers: { Authorization: `Bearer ${token()}`, "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
    if (!res.ok) {
      setWorkspaceError(await res.text())
      return false
    }
    return true
  }

  async function deleteWorkspaceApp(name: string) {
    if (!workspace) return
    setWorkspaceError("")
    const res = await fetch(`${hubUrl}${workspaceGitHubPath}?name=${encodeURIComponent(name)}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${token()}` },
    })
    if (!res.ok) {
      setWorkspaceError(await res.text())
      return
    }
    await loadWorkspaceApps()
  }

  return { workspaceApps, workspaceLoading, workspaceError, loadWorkspaceApps, saveWorkspaceApp, deleteWorkspaceApp }
}
