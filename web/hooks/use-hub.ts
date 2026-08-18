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
import { expandSummaryMessage, selectTrailingSummaries } from "@/lib/activity-prefetch"
import {
  isDuplicateLiveActivity,
  isLiveSegmentCoveredByDurable,
  isTerminalAssistantMessage,
  isTransientMessage,
  pruneOldestLiveActivities,
} from "@/lib/messages"
import { useTypewriter, type TypewriterState } from "@/hooks/use-typewriter"
import { noteOutput } from "@/hooks/use-last-output"

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
  /**
   * Expand the trailing activity_summary placeholders of a claw's cached
   * timeline into real activity rows (board cards need step data without user
   * interaction). Bounded per claw; returns false when `cancelled` interrupted
   * it so the caller can retry later. No-op when nothing is unexpanded.
   */
  prefetchTrailingActivity: (clawId: string, cancelled?: () => boolean) => Promise<boolean>
  setPinned: (clawId: string, pinned: boolean) => void
  setUnreadCount: (clawId: string, count: number) => void
  refreshClaws: () => Promise<void>
  reorderClaws: (ids: string[]) => void
}

const ORDER_KEY = "elasticclaw_claw_order"

// ── localStorage message cache ──────────────────────────────────────────────
const MESSAGES_KEY = "elasticclaw_messages"
const MAX_CACHED_PER_CLAW = 200
/** Cap live tool/activity rows in memory so floods don't bloat the client. */
const MAX_LIVE_ACTIVITIES_PER_CLAW = 200
// Board prefetch bounds: trailing summaries only, cheap enough to run for
// every visible card without noticeably delaying the initial board load. The
// row budget is the real cap; the summary count only bounds request fan-out.
const BOARD_PREFETCH_SUMMARIES = 4
const BOARD_PREFETCH_ROW_BUDGET = 40

function readCachedMessages(): Record<string, Message[]> {
  if (typeof window === "undefined") return {}
  try {
    const raw = localStorage.getItem(MESSAGES_KEY)
    if (!raw) return {}
    const parsed: Record<string, Array<{ id: string; role: string; content: string; timestamp: string }>> = JSON.parse(raw)
    const hydrated: Record<string, Message[]> = {}
    for (const [clawId, msgs] of Object.entries(parsed)) {
      hydrated[clawId] = msgs.map((m) => ({ ...m, role: m.role as Message["role"], timestamp: new Date(m.timestamp) }))
    }
    return hydrated
  } catch {
    return {}
  }
}

function clawsEqual(a: Claw, b: Claw): boolean {
  const aKeys = Object.keys(a) as Array<keyof Claw>
  const bKeys = Object.keys(b)
  if (aKeys.length !== bKeys.length) return false
  return aKeys.every((key) => {
    if (!bKeys.includes(key)) return false
    const aValue = a[key]
    const bValue = b[key]
    if (Array.isArray(aValue) && Array.isArray(bValue)) {
      return aValue.length === bValue.length && aValue.every((value, index) => value === bValue[index])
    }
    return aValue === bValue
  })
}

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
  const [messages, setMessages] = useState<Record<string, Message[]>>(readCachedMessages)
  const messagesRef = useRef<Record<string, Message[]>>({})
  const persistTimerRef = useRef<number | null>(null)
  const pendingPersistRef = useRef<Record<string, Message[]> | null>(null)
  const [connected, setConnected] = useState(false)
  const {
    displayBuffers: streamingBuffers,
    pushChunk,
    finalize: finalizeTypewriter,
    split: splitTypewriter,
    clear: clearTypewriter,
  } = useTypewriter()
  // isConfigured() is pure (server-side auth: always true), safe to read at init
  const [configured] = useState(() => isConfigured())
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

  // The cache is read once in the useState initializer above, not here — a
  // setState in an effect would paint an empty transcript first.
  const writePersistedMessages = useCallback((msgs: Record<string, Message[]>) => {
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
  const persistMessages = useCallback((msgs: Record<string, Message[]>) => {
    pendingPersistRef.current = msgs
    if (persistTimerRef.current) window.clearTimeout(persistTimerRef.current)
    persistTimerRef.current = window.setTimeout(() => {
      persistTimerRef.current = null
      const pending = pendingPersistRef.current
      pendingPersistRef.current = null
      if (pending) writePersistedMessages(pending)
    }, 1_000)
  }, [writePersistedMessages])

  useEffect(() => {
    const flushPersistedMessages = () => {
      if (persistTimerRef.current) window.clearTimeout(persistTimerRef.current)
      persistTimerRef.current = null
      const pending = pendingPersistRef.current
      pendingPersistRef.current = null
      if (pending) writePersistedMessages(pending)
    }
    window.addEventListener("pagehide", flushPersistedMessages)
    return () => {
      window.removeEventListener("pagehide", flushPersistedMessages)
      flushPersistedMessages()
    }
  }, [writePersistedMessages])
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

  // Load pinned state + order from localStorage on mount
  // (the message cache is read lazily in the useState initializer above)
  useEffect(() => {
    try {
      const saved = localStorage.getItem("elasticclaw_pinned")
      if (saved) pinnedRef.current = JSON.parse(saved)
    } catch {}
    orderRef.current = loadSavedOrder()
  }, [])

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
        const next = mapApiClaw(ac, {
          unreadCount: existing?.unreadCount ?? 0,
          isStreaming: existing?.isStreaming ?? false,
          pinned: pinnedRef.current[ac.id] ?? false,
          tags: existing?.tags,
          uptime: computeUptime(ac),
        })
        return existing && clawsEqual(existing, next) ? existing : next
      })
      // Re-apply saved order
      const order = orderRef.current
      if (order.length === 0) {
        return mapped.length === prev.length && mapped.every((claw, index) => claw === prev[index]) ? prev : mapped
      }
      const map = new Map(mapped.map((c) => [c.id, c]))
      const ordered = order.map((id) => map.get(id)).filter((c): c is Claw => !!c)
      const unordered = mapped.filter((c) => !order.includes(c.id))
      const result = [...ordered, ...unordered]
      return result.length === prev.length && result.every((claw, index) => claw === prev[index]) ? prev : result
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
        // 1. Keep non-optimistic existing durable messages not in API result (cache beyond API limit)
        // 2. Re-append any in-flight opt- messages so send() can still swap them with real UUIDs
        // 3. Preserve live activity / stream segments so opening a claw mid-turn does not
        //    wipe the in-progress transcript (stream and refresh stay content-aligned)
        const existingNonOpt = existing.filter((m) => !m.id.startsWith('opt-') && !isTransientMessage(m))
        const apiIds = new Set(msgs.map((m) => m.id))
        const cachedOnly = existingNonOpt.filter((m) => !apiIds.has(m.id))
        // Prefer the cached version of an activity_summary row over the API's:
        // the board prefetch replaces (part of) its range with real rows and
        // leaves a smaller remainder under the same deterministic id.
        // Re-adding the full placeholder would double-count the expanded rows
        // kept via cachedOnly. Summary ids are hashes of claw+range, so an id
        // match means the same range — the cached remainder is never stale.
        const existingSummaries = new Map(
          existingNonOpt.filter((m) => m.role === "activity_summary").map((m) => [m.id, m])
        )
        const apiPreferred = msgs.map((m) =>
          m.role === "activity_summary" ? existingSummaries.get(m.id) ?? m : m
        )
        const inflight = existing.filter((m) => m.id.startsWith('opt-') &&
          !msgs.some((r) => r.content === m.content && r.role === m.role))
        // Keep in-flight tool activity and live text segments that the API has
        // not yet returned. Live segments must survive tool boundaries and
        // timeline reloads — otherwise the transcript blanks until a full
        // page refresh reloads durable rows from the server.
        // Drop a live segment only when a nearby durable API row is its flush
        // (role+content+time), not when any older turn reused the same prose.
        const claimedDurableIds = new Set<string>()
        const liveTransient = existing.filter((m) => {
          if (!isTransientMessage(m)) return false
          if (m.id.startsWith("live-")) {
            return !isLiveSegmentCoveredByDurable(m, msgs, claimedDurableIds)
          }
          return true
        })
        // Dedupe by id — a cached transcript that ever picked up duplicate
        // rows (e.g. two copies of the same durable activity) must self-heal
        // here instead of re-persisting the duplication forever.
        const mergedSeen = new Set<string>()
        const merged = pruneOldestLiveActivities(
          [...apiPreferred, ...cachedOnly, ...inflight, ...liveTransient].filter((m) => {
            if (mergedSeen.has(m.id)) return false
            mergedSeen.add(m.id)
            return true
          }),
          MAX_LIVE_ACTIVITIES_PER_CLAW
        )
        // Timestamp sort with a role tie-break: a summary is stamped with the
        // newest activity it covers, so on equal times it must land AFTER the
        // covered rows — otherwise it splits a step run mid-pair.
        merged.sort((a, b) => {
          const delta = a.timestamp.getTime() - b.timestamp.getTime()
          if (delta !== 0) return delta
          return (a.role === "activity_summary" ? 1 : 0) - (b.role === "activity_summary" ? 1 : 0)
        })
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

  const prefetchTrailingActivity = useCallback(
    async (clawId: string, cancelled?: () => boolean): Promise<boolean> => {
      const picks = selectTrailingSummaries(messagesRef.current[clawId] || [], {
        maxSummaries: BOARD_PREFETCH_SUMMARIES,
        rowBudget: BOARD_PREFETCH_ROW_BUDGET,
      })
      for (const pick of picks) {
        if (cancelled?.()) return false
        const target = (messagesRef.current[clawId] || []).find((m) => m.id === pick.id)
        // A remainder emptied by a concurrent expansion keeps the id — skip it.
        if (!target?.activitySummary || target.activitySummary.count <= 0) continue
        let replacement: Message[]
        try {
          replacement = await expandSummaryMessage(clawId, target, pick.limit, {
            keepEmptyRemainder: true,
          })
        } catch (err) {
          console.warn(`Failed to prefetch activity rows for ${clawId}:`, err)
          return false
        }
        if (cancelled?.()) return false
        const durableRows = replacement.filter((m) => m.role === "activity")
        setMessages((prev) => {
          const existing = prev[clawId]
          if (!existing?.some((m) => m.id === pick.id)) return prev
          // Dedupe by id (a concurrent expansion of the same summary must not
          // insert its rows twice) and drop transient live twins now
          // represented by durable rows — otherwise steps render twice.
          const seenIds = new Set<string>()
          const merged = existing
            .flatMap((m) => (m.id === pick.id ? replacement : [m]))
            .filter((m) => {
              if (seenIds.has(m.id)) return false
              seenIds.add(m.id)
              if (
                m.role === "activity" &&
                isTransientMessage(m) &&
                durableRows.some((d) => isDuplicateLiveActivity(d, m))
              ) {
                return false
              }
              return true
            })
          const next = { ...prev, [clawId]: merged }
          persistMessages(next)
          return next
        })
      }
      return true
    },
    [persistMessages]
  )

  const connectWebSocket = useCallback(function connect() {
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
      reconnectTimerRef.current = window.setTimeout(connect, delayMs)
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
          noteOutput(claw_id)
          pushChunk(claw_id, content)
          setClaws((prev) => {
            const claw = prev.find((c) => c.id === claw_id)
            if (!claw || claw.isStreaming) return prev

            return prev.map((c) =>
              c.id === claw_id ? { ...c, isStreaming: true } : c
            )
          })
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
            call_id: payload.call_id,
            duration_ms: payload.duration_ms,
            exit_code: payload.exit_code,
            result: payload.result,
          }
          if (isUnhelpfulActivity(activity)) return
          noteOutput(clawId)
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
            const pruned = pruneOldestLiveActivities(nextMessages, MAX_LIVE_ACTIVITIES_PER_CLAW)
            const next = { ...prev, [clawId]: pruned }
            persistMessages(next)
            return next
          })
          // Bail when already streaming — this fires per chunk, and a fresh
          // claws array per chunk re-renders the whole shell.
          setClaws((prev) => {
            const target = prev.find((c) => c.id === clawId)
            if (!target || target.isStreaming) return prev
            return prev.map((c) => (c.id === clawId ? { ...c, isStreaming: true } : c))
          })
        } else if (type === "message") {
          // Final message — hold until typewriter drains, then commit
          const msg = mapApiMessage(payload)
          const clawId = payload.claw_id
          const commitFinalMessage = () => {
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
              // Keep prior live-segment rows on tool-interrupted turns. The final
              // message is often only the post-tool tail; wiping live-segments
              // blanked the transcript until a full page refresh.
              const nextMessages = withoutModelWaitActivities(prev[clawId] || [])
              if (!nextMessages.some((m) => m.id === msg.id)) {
                const lastLiveIdx = (() => {
                  for (let i = nextMessages.length - 1; i >= 0; i -= 1) {
                    if (
                      nextMessages[i].id.startsWith("live-") &&
                      nextMessages[i].role === msg.role
                    ) {
                      return i
                    }
                  }
                  return -1
                })()
                if (
                  lastLiveIdx >= 0 &&
                  nextMessages[lastLiveIdx].content === msg.content
                ) {
                  // Promote live row to durable id to avoid double-render.
                  nextMessages[lastLiveIdx] = {
                    ...nextMessages[lastLiveIdx],
                    id: msg.id,
                    timestamp: msg.timestamp,
                  }
                } else {
                  nextMessages.push(msg)
                }
              }
              const pruned = pruneOldestLiveActivities(
                nextMessages,
                MAX_LIVE_ACTIVITIES_PER_CLAW
              )
              const next = { ...prev, [clawId]: pruned }
              persistMessages(next)
              return next
            })
          }

          if (segmentedStreamRef.current[clawId]) {
            // Segments already committed as live-*/hub messages; drop typewriter tail.
            clearTypewriter(clawId)
            delete segmentedStreamRef.current[clawId]
            commitFinalMessage()
          } else {
            // Uninterrupted turn: let the typewriter finish revealing, then commit.
            finalizeTypewriter(clawId, commitFinalMessage)
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
    }
  }, [clearTypewriter, finalizeTypewriter, nextClientMessageId, persistMessages, pushChunk, refreshClaws, splitTypewriter])

  // Initialize
  useEffect(() => {
    if (!isConfigured()) return

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
    // All deps are stable useCallbacks, so this still runs once on mount
  }, [connectWebSocket, refreshClaws, refreshDependencies])

  const send = useCallback(async (clawId: string, content: string) => {
    if (!clawId || !content.trim()) return

    // Optimistically add user message
    const optimistic: Message = {
      id: nextClientMessageId("opt", clawId),
      role: "user",
      content: content.trim(),
      timestamp: new Date(),
      optimisticSelf: true,
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
    prefetchTrailingActivity,
    setPinned,
    setUnreadCount,
    refreshClaws,
    reorderClaws,
  }
}
