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
import { mapApiClaw, mapApiMessage, mapApiStatus, computeUptime } from "@/lib/mappers"
import { isTerminalAssistantMessage } from "@/lib/messages"
import { useTypewriter, type TypewriterState } from "@/hooks/use-typewriter"

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

const ORDER_KEY = "elasticclaw_claw_order"

function describeWsUrl(rawUrl: string): string {
  try {
    const url = new URL(rawUrl)
    if (url.searchParams.has("token")) {
      url.searchParams.set("token", "[redacted]")
    }
    return url.toString()
  } catch {
    return rawUrl.replace(/token=[^&]+/, "token=[redacted]")
  }
}

function isTransientMessage(message: Message): boolean {
  return message.id.startsWith("activity-") || message.id.startsWith("live-") || message.id.startsWith("thinking-")
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
    const raw = localStorage.getItem(ORDER_KEY)
    return raw ? JSON.parse(raw) : []
  } catch {
    return []
  }
}

function saveOrder(ids: string[]) {
  try {
    localStorage.setItem(ORDER_KEY, JSON.stringify(ids))
  } catch {}
}

export function useHub(selectedClawId: string | null): HubState {
  const [claws, setClaws] = useState<Claw[]>([])
  const [dependencies, setDependencies] = useState<DependencyStatus[]>([])
  const orderRef = useRef<string[]>([])
  const [messages, setMessages] = useState<Record<string, Message[]>>({})
  const messagesRef = useRef<Record<string, Message[]>>({})
  const [connected, setConnected] = useState(false)
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
  const segmentedStreamRef = useRef<Record<string, boolean>>({})
  const clientMessageSeqRef = useRef(0)

  const nextClientMessageId = useCallback((prefix: string, clawId?: string) => {
    clientMessageSeqRef.current += 1
    const scope = clawId ? `${clawId}-` : ""
    return `${prefix}-${scope}${Date.now()}-${clientMessageSeqRef.current}`
  }, [])

  // Track pinned state from localStorage
  const pinnedRef = useRef<Record<string, boolean>>({})

  // ── localStorage message cache ──────────────────────────────────────────────
  const MESSAGES_KEY = "elasticclaw_messages"
  const MAX_CACHED_PER_CLAW = 200

  const loadCachedMessages = useCallback(() => {
    try {
      const raw = localStorage.getItem(MESSAGES_KEY)
      if (!raw) return
      const parsed: Record<string, Array<{ id: string; role: string; content: string; timestamp: string }>> = JSON.parse(raw)
      const hydrated: Record<string, Message[]> = {}
      for (const [clawId, msgs] of Object.entries(parsed)) {
        hydrated[clawId] = msgs.map((m) => ({ ...m, role: m.role as Message["role"], timestamp: new Date(m.timestamp) }))
      }
      setMessages(hydrated)
    } catch {}
  }, [])

  const persistMessages = useCallback((msgs: Record<string, Message[]>) => {
    try {
      const toSave: Record<string, unknown[]> = {}
      for (const [clawId, clawMsgs] of Object.entries(msgs)) {
        // Keep last N durable messages per claw. Live stream segments and activity
        // rows are per-tab transcript state; the API stores canonical messages.
        toSave[clawId] = clawMsgs
          .filter((m) => !m.id.startsWith("opt-") && m.role !== "system" && !isTransientMessage(m))
          .slice(-MAX_CACHED_PER_CLAW)
      }
      localStorage.setItem(MESSAGES_KEY, JSON.stringify(toSave))
    } catch {}
  }, [])
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimerRef = useRef<number | null>(null)
  const reconnectAttemptRef = useRef(0)
  const shouldReconnectRef = useRef(false)
  const lastWsErrorLogRef = useRef(0)
  const pollIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const dependencyPollIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const selectedClawIdRef = useRef<string | null>(selectedClawId)

  useEffect(() => {
    selectedClawIdRef.current = selectedClawId
  }, [selectedClawId])

  useEffect(() => {
    messagesRef.current = messages
  }, [messages])

  // Load pinned state + message cache + order from localStorage on mount
  useEffect(() => {
    try {
      const saved = localStorage.getItem("elasticclaw_pinned")
      if (saved) pinnedRef.current = JSON.parse(saved)
    } catch {}
    orderRef.current = loadSavedOrder()
    loadCachedMessages()
  }, [loadCachedMessages])

  const savePinned = useCallback((pinned: Record<string, boolean>) => {
    pinnedRef.current = pinned
    try {
      localStorage.setItem("elasticclaw_pinned", JSON.stringify(pinned))
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
      const mapped: Claw[] = apiClaws.map((ac) => {
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

      setMessages((prev) => {
        const existing = prev[clawId] || []
        // Merge API result with cached state:
        // 1. Keep non-optimistic existing messages not in API result (preserves cache beyond API limit)
        // 2. Re-append any in-flight opt- messages so send() can still swap them with real UUIDs
        const existingNonOpt = existing.filter((m) => !m.id.startsWith('opt-') && !isTransientMessage(m))
        const apiIds = new Set(msgs.map((m) => m.id))
        const cachedOnly = existingNonOpt.filter((m) => !apiIds.has(m.id))
        const inflight = existing.filter((m) => m.id.startsWith('opt-') &&
          !msgs.some((r) => r.content === m.content && r.role === m.role))
        const merged = [...msgs, ...cachedOnly, ...inflight]
        merged.sort((a, b) => a.timestamp.getTime() - b.timestamp.getTime())
        const next = { ...prev, [clawId]: merged }
        persistMessages(next)
        return next
      })

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
  }, [persistMessages])

  const connectWebSocket = useCallback(() => {
    if (!shouldReconnectRef.current) return
    if (wsRef.current) {
      wsRef.current.onclose = null
      wsRef.current.onerror = null
      wsRef.current.close()
    }
    const wsUrl = getHubWsUrl()
    const safeWsUrl = describeWsUrl(wsUrl)
    let ws: WebSocket
    try {
      ws = new WebSocket(wsUrl)
    } catch (err) {
      console.error(`WS create failed for ${safeWsUrl}:`, err)
      return
    }
    wsRef.current = ws

    ws.onopen = () => {
      reconnectAttemptRef.current = 0
      setConnected(true)
    }

    ws.onclose = (event) => {
      if (wsRef.current !== ws) return
      setConnected(false)
      if (!shouldReconnectRef.current) return
      const attempt = reconnectAttemptRef.current
      reconnectAttemptRef.current += 1
      const delayMs = Math.min(30_000, 1000 * 2 ** Math.min(attempt, 5))
      if (event.code !== 1000) {
        console.warn(
          `WS closed for ${safeWsUrl}: code=${event.code} reason=${event.reason || "none"}; reconnecting in ${Math.round(delayMs / 1000)}s`
        )
      }
      if (reconnectTimerRef.current) window.clearTimeout(reconnectTimerRef.current)
      reconnectTimerRef.current = window.setTimeout(connectWebSocket, delayMs)
    }

    ws.onerror = () => {
      const nowMs = Date.now()
      if (nowMs - lastWsErrorLogRef.current < 10_000) return
      lastWsErrorLogRef.current = nowMs
      console.warn(`WS error for ${safeWsUrl}; check the Network tab for /api/ws status and close code`)
    }

    ws.onmessage = (event) => {
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
          setMessages((prev) => {
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
            const next = { ...prev, [clawId]: nextMessages }
            persistMessages(next)
            return next
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
            setMessages((prev) => {
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
              const next = { ...prev, [clawId]: nextMessages }
              persistMessages(next)
              return next
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
              setMessages((prev) => {
                const next = { ...prev, [clawId]: [...withoutModelWaitActivities(prev[clawId] || []), msg] }
                persistMessages(next)
                return next
              })
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
                      previewUrl: payload.preview_url ?? c.previewUrl,
                      previewReady: payload.preview_ready ?? c.previewReady,
                      previewExpiresAt: payload.preview_expires_at ?? c.previewExpiresAt,
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
    }
  }, [clearTypewriter, finalizeTypewriter, nextClientMessageId, persistMessages, pushChunk, refreshClaws, splitTypewriter])

  // Initialize
  useEffect(() => {
    const cfg = isConfigured()
    setConfigured(cfg)
    if (!cfg) return

    // Initial fetch + eager-load all message histories
    refreshClaws().then(() => {})
    refreshDependencies().then(() => {})

    // Poll claws frequently; dependency status is slower-moving and separately cached by the hub.
    pollIntervalRef.current = setInterval(refreshClaws, 10_000)
    dependencyPollIntervalRef.current = setInterval(refreshDependencies, 60_000)

    // Wait for token then connect WS
    shouldReconnectRef.current = true
    resolveToken().then(() => connectWebSocket())

    return () => {
      shouldReconnectRef.current = false
      if (pollIntervalRef.current) clearInterval(pollIntervalRef.current)
      if (dependencyPollIntervalRef.current) clearInterval(dependencyPollIntervalRef.current)
      if (reconnectTimerRef.current) window.clearTimeout(reconnectTimerRef.current)
      if (wsRef.current) {
        wsRef.current.onclose = null
        wsRef.current.onerror = null
        wsRef.current.close()
      }
    }
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
    setMessages((prev) => {
      const next = { ...prev, [clawId]: [...(prev[clawId] || []), optimistic] }
      persistMessages(next)
      return next
    })

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
      setMessages((prev) => {
        const msgs = prev[clawId] || []
        const replaced = msgs.map((m) => m.id === optimistic.id ? realMsg : m)
        const next = { ...prev, [clawId]: replaced }
        persistMessages(next)
        return next
      })
      // WS events will handle the response (chunk/message)
    } catch (err) {
      console.error("Failed to send message:", err)
      setClaws((prev) =>
        prev.map((c) => (c.id === clawId ? { ...c, isStreaming: false } : c))
      )
    }
  }, [nextClientMessageId, persistMessages, pushChunk])

  const createClaw = useCallback(async (req: { name: string; template: string }) => {
    const apiClaw = await apiCreateClaw({
      name: req.name,
      template: req.template,
      provider: "replicated",
    })
    const claw = mapApiClaw(apiClaw, { pinned: false, unreadCount: 0, isStreaming: false })
    setClaws((prev) => [claw, ...prev])
  }, [])

  const killClaw = useCallback(async (clawId: string) => {
    await apiKillClaw(clawId)
    setClaws((prev) => prev.filter((c) => c.id !== clawId))
    setMessages((prev) => {
      const next = { ...prev }
      delete next[clawId]
      persistMessages(next)
      return next
    })

  }, [persistMessages])

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
