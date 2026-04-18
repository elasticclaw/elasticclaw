"use client"

import { useState, useRef, useCallback, useEffect } from "react"
import { fetchMessages } from "@/lib/api"
import { mapApiMessage } from "@/lib/mappers"
import type { Message } from "@/lib/types"

const PAGE_SIZE = 50    // messages per page
const MAX_WINDOW = 50   // max messages kept in DOM

interface UseWindowedMessagesOptions {
  clawId: string
  liveMessages: Message[] // streaming/new messages from websocket
}

interface UseWindowedMessages {
  messages: Message[]
  hasOlder: boolean
  loadingOlder: boolean
  loadOlder: () => Promise<void>
  scrollRef: React.RefObject<HTMLDivElement>
  onScroll: () => void
}

export function useWindowedMessages({ clawId, liveMessages }: UseWindowedMessagesOptions): UseWindowedMessages {
  const [historicalMsgs, setHistoricalMsgs] = useState<Message[]>([])
  const [hasOlder, setHasOlder] = useState(false)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const initialLoaded = useRef(false)
  const oldestTimestamp = useRef<string | null>(null)

  // Initial load — last PAGE_SIZE messages
  useEffect(() => {
    if (initialLoaded.current || !clawId) return
    initialLoaded.current = true

    fetchMessages(clawId)
      .then((apiMsgs) => {
        const msgs = apiMsgs.map(mapApiMessage)
        setHistoricalMsgs(msgs)
        if (msgs.length > 0) {
          oldestTimestamp.current = msgs[0].timestamp
        }
        setHasOlder(apiMsgs.length >= PAGE_SIZE)
      })
      .catch(console.warn)
  }, [clawId])

  // Load older messages when user scrolls to top
  const loadOlder = useCallback(async () => {
    if (!hasOlder || loadingOlder || !oldestTimestamp.current) return
    setLoadingOlder(true)

    const el = scrollRef.current
    const prevHeight = el?.scrollHeight ?? 0

    try {
      const apiMsgs = await fetchMessages(clawId, { before: oldestTimestamp.current })
      const older = apiMsgs.map(mapApiMessage)

      if (older.length === 0) {
        setHasOlder(false)
        return
      }

      setHasOlder(apiMsgs.length >= PAGE_SIZE)
      if (older.length > 0) {
        oldestTimestamp.current = older[0].timestamp
      }

      setHistoricalMsgs((prev) => {
        const combined = [...older, ...prev]
        // Cap window — drop from bottom if too large
        return combined.length > MAX_WINDOW ? combined.slice(0, MAX_WINDOW) : combined
      })

      // Restore scroll position so prepend doesn't jump
      requestAnimationFrame(() => {
        if (el) {
          el.scrollTop = el.scrollHeight - prevHeight
        }
      })
    } catch (err) {
      console.warn("Failed to load older messages:", err)
    } finally {
      setLoadingOlder(false)
    }
  }, [clawId, hasOlder, loadingOlder])

  // Trigger loadOlder when scrolled near the top
  const onScroll = useCallback(() => {
    const el = scrollRef.current
    if (!el || loadingOlder || !hasOlder) return
    if (el.scrollTop < 200) {
      loadOlder()
    }
  }, [loadOlder, loadingOlder, hasOlder])

  // Merge historical + live, deduplicate by id, cap window
  const messages = (() => {
    const seen = new Set<string>()
    const all: Message[] = []
    for (const m of historicalMsgs) {
      if (!seen.has(m.id)) { seen.add(m.id); all.push(m) }
    }
    for (const m of liveMessages) {
      if (!seen.has(m.id)) { seen.add(m.id); all.push(m) }
    }
    // If too many, drop oldest
    return all.length > MAX_WINDOW ? all.slice(all.length - MAX_WINDOW) : all
  })()

  return { messages, hasOlder, loadingOlder, loadOlder, scrollRef, onScroll }
}
