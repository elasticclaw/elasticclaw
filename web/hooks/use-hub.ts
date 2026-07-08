"use client"

import { useState, useEffect, useRef, useCallback } from "react"
import type { AgentActivity, ApiClaw, Claw, DependencyStatus, Message } from "@/lib/types"
import {
  fetchClaws,
  fetchDependencyStatus,
  fetchMessageTimeline,
  sendMessage as apiSendMessage,
  createClaw as apiCreateClaw,
  killClaw as apiKillClaw,
  getHubWsUrl,
  resolveToken,
  isConfigured,
} from "@/lib/api"
import { mapApiClaw, mapApiMessage, mapApiStatus, computeUptime, isDeletedClaw } from "@/lib/mappers"
import { isTerminalAssistantMessage } from "@/lib/messages"
import {
  CLAW_POLL_INTERVAL_MS,
  DEPENDENCY_POLL_INTERVAL_MS,
  STORAGE_KEY_CLAW_ORDER,
  STORAGE_KEY_PINNED,
} from "@/lib/constants"
import { useTypewriter, type TypewriterState } from "@/hooks/use-typewriter"
import { useWebSocket } from "@/hooks/use-websocket"
import { useMessageCache, isTransientMessage } from "@/hooks/use-message-cache"

export interface HubState {
  claws: Claw[]
  dependencies: DependencyStatus[]
  downtimeDependencies: DependencyStatus[]
  messages: Record<string, Message[]>
  streamingBuffers: Record<string, TypewriterState>
  connected: boolean
  configured: boolean
  loading: boolean
  hubError: string | null
  send: (clawId: string, content: string) => Promise<void>
  createClaw: (req: { name: string; template: string }) => Promise<void>
  killClaw: (clawId: string) => Promise<void>
  loadMessages: (clawId: string) => Promise<void>
  setPinned: (clawId: string, pinned: boolean) => void
  setUnreadCount: (clawId: string, count: number) => void
  refreshClaws: () => Promise<void>
  reorderClaws: (ids: string[]) => void
}

function formatActivityContent(activity: AgentActivity): string {
  if (activity.error) return activity.error
  if (activity.command) return activity.command
  if (activity.path) return activity.path
  if (activity.url) return activity.url
  if (activity.detail) return activity.detail
  if (activity.message) return activity.message
  if (activity.tool) return activity.tool
  switch (activity.kind) {
    case "model_started":
      return "Waiting on model response"
    case "tool":
      return "Tool activity"
    default:
      return activity.phase || activity.stream || "Activity"
  }
}

function isUnhelpfulActivity(activity: AgentActivity): boolean {
  return activity.kind === "still_working" || Boolean(activity.message?.startsWith("No streamed output")) || Boolean(activity.error?.startsWith("No streamed output"))
}

function withoutModelWaitActivities(messages: Message[]): Message[] {
  return messages.filter((message) => message.activity?.kind !== "model_started")
}

function loadSavedOrder(): string[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY_CLAW_ORDER)
    return raw ? JSON.parse(raw) : []
  } catch {
    return []
  }
}

function saveOrder(ids: string[]) {
  try {
    localStorage.setItem(STORAGE_KEY_CLAW_ORDER, JSON.stringify(ids))
  } catch {}
}

/**
 * useHub — composition root for the hub UI state: claw list (poll + reorder +
 * pin), dependency status, hub WebSocket events, and the message transcript.
 * Connection management lives in useWebSocket; message persistence and the
 * durable/transient merge live in useMessageCache.
 */
export function useHub(selectedClawId: string | null): HubState {
  const [claws, setClaws] = useState<Claw[]>([])
  const [dependencies, setDependencies] = useState<DependencyStatus[]>([])
  const orderRef = useRef<string[]>([])
  const { messages, messagesRef, loadCachedMessages, updateMessages, mergeTimeline, removeClaw } = useMessageCache()
  const {
    displayBuffers: streamingBuffers,
    pushChunk,
    finalize: finalizeTypewriter,
    split: splitTypewriter,
    clear: clearTypewriter,
  } = useTypewriter()
  const [configured, setConfigured] = useState(false)
  const [loading, setLoading] = useState(true) // true until first claws fetch completes
  const [hubError, setHubError] = useState<string | null>(null)
  const [wsEnabled, setWsEnabled] = useState(false)
  const segmentedStreamRef = useRef<Record<string, boolean>>({})
  const clientMessageSeqRef = useRef(0)

  const nextClientMessageId = useCallback((prefix: string, clawId?: string) => {
    clientMessageSeqRef.current += 1
    const scope = clawId ? `${clawId}-` : ""
    return `${prefix}-${scope}${Date.now()}-${clientMessageSeqRef.current}`
  }, [])

  // Track pinned state from localStorage
  const pinnedRef = useRef<Record<string, boolean>>({})

  const pollIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const dependencyPollIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const selectedClawIdRef = useRef<string | null>(selectedClawId)

  useEffect(() => {
    selectedClawIdRef.current = selectedClawId
  }, [selectedClawId])

  // Load pinned state + message cache + order from localStorage on mount
  useEffect(() => {
    try {
      const saved = localStorage.getItem(STORAGE_KEY_PINNED)
      if (saved) pinnedRef.current = JSON.parse(saved)
    } catch {}
    orderRef.current = loadSavedOrder()
    loadCachedMessages()
  }, [loadCachedMessages])

  const savePinned = useCallback((pinned: Record<string, boolean>) => {
    pinnedRef.current = pinned
    try {
      localStorage.setItem(STORAGE_KEY_PINNED, JSON.stringify(pinned))
    } catch {}
  }, [])

  const setPinned = useCallback((clawId: string, pinned: boolean) => {
    const next = { ...pinnedRef.current, [clawId]: pinned }
    savePinned(next)
    setClaws((prev) =>
      prev.map((c) => (c.id === clawId ? { ...c, pinned } : c))
    )
  }, [savePinned])

  const setUnreadCount = useCallback((clawId: string, count: number) => {
    setClaws((prev) =>
      prev.map((c) => (c.id === clawId ? { ...c, unreadCount: count } : c))
    )
  }, [])

  // Reorder claws — accepts new ordered list of IDs and persists it
  const reorderClaws = useCallback((ids: string[]) => {
    orderRef.current = ids
    saveOrder(ids)
    setClaws((prev) => {
      const map = new Map(prev.map((c) => [c.id, c]))
      const ordered = ids.map((id) => map.get(id)).filter((c): c is Claw => !!c)
      const rest = prev.filter((c) => !ids.includes(c.id))
      return [...ordered, ...rest]
    })
  }, [])

  // Merge fresh API claws into state, preserving UI-only fields (including order)
  const mergeClaws = useCallback((apiClaws: ApiClaw[]) => {
    setClaws((prev) => {
      const prevMap = new Map(prev.map((c) => [c.id, c]))
      // Deleted claws never render — the API filters them from list
      // responses, but a stale cache or WS race could still surface one.
      const mapped: Claw[] = apiClaws.filter((ac) => !isDeletedClaw(ac)).map((ac) => {
        const existing = prevMap.get(ac.id)
        return mapApiClaw(ac, {
          unreadCount: existing?.unreadCount ?? 0,
          isStreaming: existing?.isStreaming ?? false,
          pinned: pinnedRef.current[ac.id] ?? false,
          tags: existing?.tags,
          uptime: computeUptime(ac),
        })
      })
      // Re-apply saved order
      const order = orderRef.current
      if (order.length === 0) return mapped
      const map = new Map(mapped.map((c) => [c.id, c]))
      const ordered = order.map((id) => map.get(id)).filter((c): c is Claw => !!c)
      const unordered = mapped.filter((c) => !order.includes(c.id))
      return [...ordered, ...unordered]
    })
  }, [])

  const refreshClaws = useCallback(async (): Promise<void> => {
    try {
      const apiClaws = await fetchClaws()
      mergeClaws(apiClaws)
      setHubError(null)
      setLoading(false)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      setHubError(msg)
      setLoading(false)
    }
  }, [mergeClaws])

  const refreshDependencies = useCallback(async (): Promise<void> => {
    try {
      const snapshot = await fetchDependencyStatus()
      const sorted = [...(snapshot.dependencies || [])].sort((a, b) => {
        if (a.kind !== b.kind) return a.kind.localeCompare(b.kind)
        return a.name.localeCompare(b.name)
      })
      setDependencies(sorted)
    } catch (err) {
      console.warn("Failed to load dependency status:", err)
    }
  }, [])

  const loadMessages = useCallback(async (clawId: string) => {
    try {
      const apiMsgs = await fetchMessageTimeline(clawId)
      const msgs = apiMsgs.map(mapApiMessage)

      // Capture existing IDs before updating so we can diff outside the updater.
      // React 18 batches state updates — side effects inside updaters are unreliable.
      const existingIds = new Set((messagesRef.current[clawId] || []).map((m) => m.id))
      const newClawMsgs = msgs.filter((m) => !existingIds.has(m.id) && m.role !== 'user' && m.role !== 'system')

      mergeTimeline(clawId, msgs)

      if (newClawMsgs.length > 0 && selectedClawIdRef.current !== clawId) {
        setClaws((prevClaws) =>
          prevClaws.map((c) =>
            c.id === clawId && selectedClawIdRef.current !== clawId
              ? { ...c, unreadCount: c.unreadCount + newClawMsgs.length }
              : c
          )
        )
      }
    } catch (err) {
      console.warn(`Failed to load messages for ${clawId}:`, err)
    }
  }, [mergeTimeline, messagesRef])

  const handleWsMessage = useCallback((event: MessageEvent) => {
    try {
      const data = JSON.parse(event.data)
      const { type, payload } = data

      if (type === "chunk") {
        // Streaming chunk — feed into typewriter
        const { claw_id, content } = payload
        pushChunk(claw_id, content)
        setClaws((prev) =>
          prev.map((c) =>
              c.id === claw_id ? { ...c, isStreaming: true } : c
          )
        )
      } else if (type === "agent_activity") {
        const clawId = payload.claw_id
        if (!clawId) return
        const activity: AgentActivity = {
          kind: payload.kind || "activity",
          stream: payload.stream,
          phase: payload.phase,
          tool: payload.tool,
          detail: payload.detail,
          command: payload.command,
          path: payload.path,
          url: payload.url,
          message: payload.message,
          error: payload.error,
        }
        if (isUnhelpfulActivity(activity)) return
        const currentMessages = messagesRef.current[clawId] || []
        const lastDurable = [...currentMessages].reverse().find((message) => !isTransientMessage(message) && message.role !== "activity")
        if (activity.kind === "model_started" && lastDurable && isTerminalAssistantMessage(lastDurable)) return
        const segment = splitTypewriter(clawId)
        const createdAt = payload.created_at ? new Date(payload.created_at) : new Date()
        const segmentId = segment.trim() ? nextClientMessageId("live-segment", clawId) : null
        const activityId = nextClientMessageId("activity", clawId)
        segmentedStreamRef.current[clawId] = true
        updateMessages((prev) => {
          const nextMessages = [...(prev[clawId] || [])]
          if (segmentId) {
            nextMessages.push({
              id: segmentId,
              role: "claw",
              content: segment,
              timestamp: createdAt,
            })
          }
          nextMessages.push({
            id: activityId,
            role: "activity",
            content: formatActivityContent(activity),
            activity,
            timestamp: createdAt,
          })
          return { ...prev, [clawId]: nextMessages }
        })
        setClaws((prev) =>
          prev.map((c) =>
            c.id === clawId ? { ...c, isStreaming: true } : c
          )
        )
      } else if (type === "message") {
        // Final message — hold until typewriter drains, then commit
        const msg = mapApiMessage(payload)
        const clawId = payload.claw_id
        if (segmentedStreamRef.current[clawId]) {
          const tail = clearTypewriter(clawId)
          delete segmentedStreamRef.current[clawId]
          const tailId = tail.trim() ? nextClientMessageId("live-segment", clawId) : null
          // Called once typewriter is fully drained — safe to add final message
          setClaws((prev) =>
            prev.map((c) =>
              c.id === clawId
                ? {
                    ...c,
                    isStreaming: false,
                    unreadCount:
                      selectedClawIdRef.current !== clawId && msg.role === "claw"
                        ? c.unreadCount + 1
                        : c.unreadCount,
                  }
                : c
            )
          )
          updateMessages((prev) => {
            const nextMessages = withoutModelWaitActivities(prev[clawId] || [])
            const hasLiveSegment = nextMessages.some((m) => m.id.startsWith(`live-segment-${clawId}-`))
            if (tailId) {
              nextMessages.push({
                id: tailId,
                role: "claw",
                content: tail,
                timestamp: msg.timestamp,
              })
            } else if (!hasLiveSegment && msg.content.trim()) {
              nextMessages.push(msg)
            }
            return { ...prev, [clawId]: nextMessages }
          })
        } else {
          finalizeTypewriter(clawId, () => {
            // Called once typewriter is fully drained — safe to add final message
            setClaws((prev) =>
              prev.map((c) =>
                c.id === clawId
                  ? {
                      ...c,
                      isStreaming: false,
                      unreadCount:
                        selectedClawIdRef.current !== clawId && msg.role === "claw"
                          ? c.unreadCount + 1
                          : c.unreadCount,
                    }
                  : c
              )
            )
            updateMessages((prev) => ({ ...prev, [clawId]: [...withoutModelWaitActivities(prev[clawId] || []), msg] }))
          })
        }
      } else if (type === "claw_status") {
        const { claw_id, status } = payload
        if (status === "deleted") {
          // Remove immediately — don't wait for next poll
          setClaws((prev) => prev.filter((c) => c.id !== claw_id))
        } else {
          setClaws((prev) =>
            prev.map((c) =>
              c.id === claw_id
                ? {
                    ...c,
                    status: mapApiStatus(status),
                    isStreaming: status !== "connected" ? false : c.isStreaming,
                    reason: status === "error" ? payload.reason : undefined,
                    bootstrap_status: status === "connected" || status === "error" ? undefined : payload.bootstrap_status ?? c.bootstrap_status,
                    githubIssueId: payload.github_issue_id ?? c.githubIssueId,
                    githubIssueUrl: payload.github_issue_url ?? c.githubIssueUrl,
                  }
                : c
            )
          )
          // If a new claw came online that we don't know about, refresh
          setClaws((prev) => {
            if (!prev.find((c) => c.id === claw_id)) {
              refreshClaws()
            }
            return prev
          })
        }
      }
    } catch (err) {
      console.warn("Failed to parse WS message:", err)
    }
  }, [clearTypewriter, finalizeTypewriter, messagesRef, nextClientMessageId, pushChunk, refreshClaws, splitTypewriter, updateMessages])

  const { connected } = useWebSocket({
    getUrl: getHubWsUrl,
    enabled: wsEnabled,
    onMessage: handleWsMessage,
  })

  // Initialize
  useEffect(() => {
    const cfg = isConfigured()
    setConfigured(cfg)
    if (!cfg) return

    // Initial fetch + eager-load all message histories
    refreshClaws().then(() => {})
    refreshDependencies().then(() => {})

    // Poll claws frequently; dependency status is slower-moving and separately cached by the hub.
    pollIntervalRef.current = setInterval(refreshClaws, CLAW_POLL_INTERVAL_MS)
    dependencyPollIntervalRef.current = setInterval(refreshDependencies, DEPENDENCY_POLL_INTERVAL_MS)

    // Wait for token then connect WS
    resolveToken().then(() => setWsEnabled(true))

    return () => {
      setWsEnabled(false)
      if (pollIntervalRef.current) clearInterval(pollIntervalRef.current)
      if (dependencyPollIntervalRef.current) clearInterval(dependencyPollIntervalRef.current)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []) // run once on mount

  const send = useCallback(async (clawId: string, content: string) => {
    if (!clawId || !content.trim()) return

    // Optimistically add user message
    const optimistic: Message = {
      id: nextClientMessageId("opt", clawId),
      role: "user",
      content: content.trim(),
      timestamp: new Date(),
    }
    updateMessages((prev) => ({ ...prev, [clawId]: [...(prev[clawId] || []), optimistic] }))

    setClaws((prev) =>
      prev.map((c) => (c.id === clawId ? { ...c, isStreaming: true } : c))
    )
    // Push empty chunk to create a typewriter entry immediately — shows thinking dots
    pushChunk(clawId, "")

    try {
      const sent = await apiSendMessage(clawId, content.trim())
      // Replace the optimistic message with the real one from the DB
      // so it survives cache persistence (opt- IDs are filtered out)
      const realMsg = mapApiMessage(sent)
      updateMessages((prev) => {
        const msgs = prev[clawId] || []
        const replaced = msgs.map((m) => m.id === optimistic.id ? realMsg : m)
        return { ...prev, [clawId]: replaced }
      })
      // WS events will handle the response (chunk/message)
    } catch (err) {
      console.error("Failed to send message:", err)
      setClaws((prev) =>
        prev.map((c) => (c.id === clawId ? { ...c, isStreaming: false } : c))
      )
    }
  }, [nextClientMessageId, pushChunk, updateMessages])

  const createClaw = useCallback(async (req: { name: string; template: string }) => {
    const apiClaw = await apiCreateClaw({
      name: req.name,
      template_name: req.template,
      provider: "replicated",
    })
    const claw = mapApiClaw(apiClaw, { pinned: false, unreadCount: 0, isStreaming: false })
    setClaws((prev) => [claw, ...prev])
  }, [])

  const killClaw = useCallback(async (clawId: string) => {
    await apiKillClaw(clawId)
    setClaws((prev) => prev.filter((c) => c.id !== clawId))
    removeClaw(clawId)
  }, [removeClaw])

  const downtimeDependencies = dependencies.filter((dependency) => dependency.status === "downtime")

  return {
    claws,
    dependencies,
    downtimeDependencies,
    messages,
    streamingBuffers,
    connected,
    configured,
    loading,
    hubError,
    send,
    createClaw,
    killClaw,
    loadMessages,
    setPinned,
    setUnreadCount,
    refreshClaws,
    reorderClaws,
  }
}
