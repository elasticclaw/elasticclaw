"use client"

import { useState, useEffect, useRef, useCallback } from "react"
import type { Claw, Message, CreateClawRequest } from "@/lib/types"
import {
  fetchClaws,
  fetchMessages,
  sendMessage as apiSendMessage,
  createClaw as apiCreateClaw,
  killClaw as apiKillClaw,
  getHubWsUrl,
  resolveToken,
  isConfigured,
} from "@/lib/api"
import { mapApiClaw, mapApiMessage, mapApiStatus, computeUptime } from "@/lib/mappers"
import type { ApiClaw } from "@/lib/types"
import { useTypewriter, type TypewriterState } from "@/hooks/use-typewriter"

export interface HubState {
  claws: Claw[]
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
}

export function useHub(selectedClawId: string | null): HubState {
  const [claws, setClaws] = useState<Claw[]>([])
  const [messages, setMessages] = useState<Record<string, Message[]>>({})
  const [connected, setConnected] = useState(false)
  const { displayBuffers: streamingBuffers, pushChunk, finalize: finalizeTypewriter } = useTypewriter()
  const [configured, setConfigured] = useState(false)
  const [loading, setLoading] = useState(true) // true until first claws fetch completes
  const [hubError, setHubError] = useState<string | null>(null)

  // Track which claws have had messages loaded from API
  const loadedMessages = useRef<Set<string>>(new Set())
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
        // Keep last N messages per claw, skip optimistic/system
        toSave[clawId] = clawMsgs
          .filter((m) => !m.id.startsWith("opt-") && m.role !== "system")
          .slice(-MAX_CACHED_PER_CLAW)
      }
      localStorage.setItem(MESSAGES_KEY, JSON.stringify(toSave))
    } catch {}
  }, [])
  const wsRef = useRef<WebSocket | null>(null)
  const pollIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const selectedClawIdRef = useRef<string | null>(selectedClawId)

  useEffect(() => {
    selectedClawIdRef.current = selectedClawId
  }, [selectedClawId])

  // Load pinned state + message cache from localStorage on mount
  useEffect(() => {
    try {
      const saved = localStorage.getItem("elasticclaw_pinned")
      if (saved) pinnedRef.current = JSON.parse(saved)
    } catch {}
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

  // Merge fresh API claws into state, preserving UI-only fields
  const mergeClaws = useCallback((apiClaws: ApiClaw[]) => {
    setClaws((prev) => {
      const prevMap = new Map(prev.map((c) => [c.id, c]))
      const next: Claw[] = apiClaws.map((ac) => {
        const existing = prevMap.get(ac.id)
        return mapApiClaw(ac, {
          unreadCount: existing?.unreadCount ?? 0,
          isStreaming: existing?.isStreaming ?? false,
          pinned: pinnedRef.current[ac.id] ?? false,
          tags: existing?.tags,
          // Update uptime live
          uptime: computeUptime(ac),
        })
      })
      return next
    })
  }, [])

  const refreshClaws = useCallback(async () => {
    try {
      const apiClaws = await fetchClaws()
      mergeClaws(apiClaws)
      setHubError(null)
      setLoading(false)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      setHubError(msg)
      setLoading(false)
      return []
    }
  }, [mergeClaws])

  const loadMessages = useCallback(async (clawId: string) => {
    if (loadedMessages.current.has(clawId)) return
    loadedMessages.current.add(clawId)
    try {
      const apiMsgs = await fetchMessages(clawId)
      const msgs = apiMsgs.map(mapApiMessage)
      setMessages((prev) => {
        const next = { ...prev, [clawId]: msgs }
        persistMessages(next)
        return next
      })
    } catch (err) {
      console.warn(`Failed to load messages for ${clawId}:`, err)
    }
  }, [persistMessages])

  const connectWebSocket = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close()
    }
    const wsUrl = getHubWsUrl()
    let ws: WebSocket
    try {
      ws = new WebSocket(wsUrl)
    } catch (err) {
      console.error("WS create failed:", err)
      return
    }
    wsRef.current = ws

    ws.onopen = () => {
      setConnected(true)
    }

    ws.onclose = () => {
      setConnected(false)
      // Reconnect after 3s
      setTimeout(connectWebSocket, 3000)
    }

    ws.onerror = (err) => {
      console.warn("WS error:", err)
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
        } else if (type === "message") {
          // Final message — hold until typewriter drains, then commit
          const msg = mapApiMessage(payload)
          const clawId = payload.claw_id
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
              const next = { ...prev, [clawId]: [...(prev[clawId] || []), msg] }
              persistMessages(next)
              return next
            })
          })
        } else if (type === "claw_status") {
          const { claw_id, status } = payload
          setClaws((prev) =>
            prev.map((c) =>
              c.id === claw_id
                ? { ...c, status: mapApiStatus(status), isStreaming: status !== "connected" ? false : c.isStreaming }
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
        } else if (type === "claw_error") {
          const { claw_id, error } = payload
          console.warn(`Claw ${claw_id} error:`, error)
          setClaws((prev) =>
            prev.map((c) =>
              c.id === claw_id ? { ...c, status: "error", isStreaming: false } : c
            )
          )
        }
      } catch (err) {
        console.warn("Failed to parse WS message:", err)
      }
    }
  }, [mergeClaws, refreshClaws])

  // Initialize
  useEffect(() => {
    const cfg = isConfigured()
    setConfigured(cfg)
    if (!cfg) return

    // Initial fetch + eager-load all message histories
    refreshClaws().then(() => {
      // loadMessages for each claw is triggered via mergeClaws → selectedClawId changes
    })

    // Poll every 30s
    pollIntervalRef.current = setInterval(refreshClaws, 30_000)

    // Wait for token then connect WS
    resolveToken().then(() => connectWebSocket())

    return () => {
      if (pollIntervalRef.current) clearInterval(pollIntervalRef.current)
      if (wsRef.current) wsRef.current.close()
    }
  }, []) // run once on mount

  const send = useCallback(async (clawId: string, content: string) => {
    if (!clawId || !content.trim()) return

    // Optimistically add user message
    const optimistic: Message = {
      id: `opt-${Date.now()}`,
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
      await apiSendMessage(clawId, content.trim())
      // WS events will handle the response (chunk/message)
    } catch (err) {
      console.error("Failed to send message:", err)
      setClaws((prev) =>
        prev.map((c) => (c.id === clawId ? { ...c, isStreaming: false } : c))
      )
    }
  }, [])

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
    loadedMessages.current.delete(clawId)
  }, [persistMessages])

  return {
    claws,
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
  }
}
