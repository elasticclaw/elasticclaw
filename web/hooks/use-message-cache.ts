"use client"

import { useCallback, useEffect, useRef, useState, type MutableRefObject } from "react"
import type { Message } from "@/lib/types"
import { MAX_CACHED_MESSAGES_PER_CLAW, STORAGE_KEY_MESSAGES } from "@/lib/constants"

/**
 * Transient messages are per-tab transcript state (live stream segments,
 * activity rows, thinking placeholders); the API stores canonical messages.
 */
export function isTransientMessage(message: Message): boolean {
  return message.id.startsWith("activity-") || message.id.startsWith("live-") || message.id.startsWith("thinking-")
}

/**
 * Merges a fresh API timeline into the cached transcript for one claw:
 * 1. Keep non-optimistic cached messages missing from the API result
 *    (preserves history beyond the API fetch limit).
 * 2. Re-append in-flight `opt-` messages that the API has not confirmed yet,
 *    so send() can still swap them with real UUIDs.
 */
export function mergeMessageTimeline(existing: Message[], fresh: Message[]): Message[] {
  const existingDurable = existing.filter((m) => !m.id.startsWith("opt-") && !isTransientMessage(m))
  const freshIds = new Set(fresh.map((m) => m.id))
  const cachedOnly = existingDurable.filter((m) => !freshIds.has(m.id))
  const inflight = existing.filter(
    (m) => m.id.startsWith("opt-") && !fresh.some((r) => r.content === m.content && r.role === m.role)
  )
  const merged = [...fresh, ...cachedOnly, ...inflight]
  merged.sort((a, b) => a.timestamp.getTime() - b.timestamp.getTime())
  return merged
}

export interface MessageCache {
  messages: Record<string, Message[]>
  /** Always-current snapshot for reads inside event handlers. */
  messagesRef: MutableRefObject<Record<string, Message[]>>
  /** Hydrates the state from localStorage (call once on mount). */
  loadCachedMessages: () => void
  /** setState + persist: every durable mutation goes through here. */
  updateMessages: (updater: (prev: Record<string, Message[]>) => Record<string, Message[]>) => void
  /** Merges a fresh API timeline for one claw (durable/transient merge). */
  mergeTimeline: (clawId: string, fresh: Message[]) => void
  /** Drops a claw's transcript (e.g. after kill). */
  removeClaw: (clawId: string) => void
}

/**
 * useMessageCache — owns the per-claw message map and its localStorage
 * persistence. Only durable messages are persisted: optimistic (`opt-`),
 * system and transient messages are filtered out, and each claw keeps at
 * most MAX_CACHED_MESSAGES_PER_CLAW entries.
 */
export function useMessageCache(): MessageCache {
  const [messages, setMessages] = useState<Record<string, Message[]>>({})
  const messagesRef = useRef<Record<string, Message[]>>({})

  useEffect(() => {
    messagesRef.current = messages
  }, [messages])

  const persistMessages = useCallback((msgs: Record<string, Message[]>) => {
    try {
      const toSave: Record<string, unknown[]> = {}
      for (const [clawId, clawMsgs] of Object.entries(msgs)) {
        toSave[clawId] = clawMsgs
          .filter((m) => !m.id.startsWith("opt-") && m.role !== "system" && !isTransientMessage(m))
          .slice(-MAX_CACHED_MESSAGES_PER_CLAW)
      }
      localStorage.setItem(STORAGE_KEY_MESSAGES, JSON.stringify(toSave))
    } catch {}
  }, [])

  const loadCachedMessages = useCallback(() => {
    try {
      const raw = localStorage.getItem(STORAGE_KEY_MESSAGES)
      if (!raw) return
      const parsed: Record<string, Array<{ id: string; role: string; content: string; timestamp: string }>> =
        JSON.parse(raw)
      const hydrated: Record<string, Message[]> = {}
      for (const [clawId, msgs] of Object.entries(parsed)) {
        hydrated[clawId] = msgs.map((m) => ({ ...m, role: m.role as Message["role"], timestamp: new Date(m.timestamp) }))
      }
      setMessages(hydrated)
    } catch {}
  }, [])

  const updateMessages = useCallback(
    (updater: (prev: Record<string, Message[]>) => Record<string, Message[]>) => {
      setMessages((prev) => {
        const next = updater(prev)
        persistMessages(next)
        return next
      })
    },
    [persistMessages]
  )

  const mergeTimeline = useCallback(
    (clawId: string, fresh: Message[]) => {
      updateMessages((prev) => ({ ...prev, [clawId]: mergeMessageTimeline(prev[clawId] || [], fresh) }))
    },
    [updateMessages]
  )

  const removeClaw = useCallback(
    (clawId: string) => {
      updateMessages((prev) => {
        const next = { ...prev }
        delete next[clawId]
        return next
      })
    },
    [updateMessages]
  )

  return { messages, messagesRef, loadCachedMessages, updateMessages, mergeTimeline, removeClaw }
}
