"use client"

import { useCallback, useEffect, useState } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import type { WorkspaceGitHubAppView } from "./types"

export type TrackerType = "linear" | "shortcut" | "github-issues" | "jira"

export interface TrackerItem {
  type: TrackerType
  workspace: string
  tokenSet: boolean
  baseUrl?: string
  username?: string
}

/**
 * Loads the issue trackers configured on a workspace
 * (/api/workspaces/{ws}/issue-trackers) plus the GitHub owner hint used to
 * prefill the fine-grained PAT link.
 */
export function useIssueTrackers(selectedWorkspace: string) {
  const [workspaceTrackers, setWorkspaceTrackers] = useState<TrackerItem[]>([])
  const [githubOwnerHint, setGithubOwnerHint] = useState("")
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const hubUrl = getHubUrl()
  const authToken = () => getAuthToken() || ""
  const issueTrackersPath = selectedWorkspace ? `/api/workspaces/${encodeURIComponent(selectedWorkspace)}/issue-trackers` : ""

  const loadTrackers = useCallback(async () => {
    if (!selectedWorkspace) return
    setLoading(true)
    setError("")
    try {
      const res = await fetch(`${hubUrl}${issueTrackersPath}`, { headers: { Authorization: `Bearer ${authToken()}` } })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      setWorkspaceTrackers(data.issueTrackers || [])
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load issue trackers")
    } finally {
      setLoading(false)
    }
  }, [hubUrl, issueTrackersPath, selectedWorkspace])

  useEffect(() => {
    loadTrackers()
  }, [loadTrackers])

  useEffect(() => {
    if (!selectedWorkspace) return
    async function loadGitHubOwnerHint() {
      try {
        const res = await fetch(`${hubUrl}/api/workspaces/${encodeURIComponent(selectedWorkspace)}/github-apps`, { headers: { Authorization: `Bearer ${authToken()}` } })
        if (!res.ok) return
        const data = await res.json()
        const apps = (data.githubApps || []) as WorkspaceGitHubAppView[]
        const owners = new Set<string>()
        for (const app of apps) {
          for (const owner of app.installations || []) {
            if (owner) owners.add(owner)
          }
        }
        setGithubOwnerHint(owners.size === 1 ? Array.from(owners)[0] : "")
      } catch {
        setGithubOwnerHint("")
      }
    }
    loadGitHubOwnerHint()
  }, [hubUrl, selectedWorkspace])

  return { workspaceTrackers, githubOwnerHint, loading, error, setError, loadTrackers, issueTrackersPath }
}
