"use client"

import { useEffect, useEffectEvent, useState, type RefObject } from "react"
import { fetchAIChatSources, type AIChatSources } from "@/lib/api"

type SourcesResult = { workspace: string; revision: number; data?: AIChatSources; error?: string }

// Loads the sources of the selected workspace ("" lets the server pick one).
// `current` is undefined while the selected workspace/revision is loading.
// If a load shows the selected workspace was removed and no conversation is
// open (threadRef is empty), it falls back to the first remaining workspace
// and calls onWorkspaceRemoved.
export function useChatSources(threadRef: RefObject<string>, onWorkspaceRemoved: () => void) {
  const [request, setRequest] = useState({ workspace: "", revision: 0, refresh: false })
  const { workspace, revision, refresh: forceRefresh } = request
  const setWorkspace = (workspace: string) => setRequest((previous) => ({ ...previous, workspace, refresh: false }))
  const [result, setResult] = useState<SourcesResult>()
  // A default ("") load already answers for the workspace the server picked.
  const current = result?.revision === revision && (result.workspace === workspace || result.data?.workspace === workspace) ? result : undefined
  const workspaceRemoved = useEffectEvent(onWorkspaceRemoved)

  useEffect(() => {
    let cancelled = false
    fetchAIChatSources(workspace, forceRefresh).then(
      (data) => {
        if (cancelled) return
        if (workspace && !threadRef.current && !data.workspaces.includes(workspace)) {
          setWorkspace(data.workspaces[0] ?? "")
          workspaceRemoved()
          return
        }
        setResult({ workspace, revision, data })
      },
      () => { if (!cancelled) setResult({ workspace, revision, error: "Unable to load workspace sources." }) },
    )
    return () => { cancelled = true }
  }, [workspace, revision, forceRefresh, threadRef])

  const refresh = () => setRequest((previous) => ({ ...previous, revision: previous.revision + 1, refresh: true }))
  // Keep the popover visible during a refresh, but never show another workspace's sources.
  const data = result?.workspace === workspace || result?.data?.workspace === workspace ? result?.data : undefined
  return { current, data, setWorkspace, refresh }
}
