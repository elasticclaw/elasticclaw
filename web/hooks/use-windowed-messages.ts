"use client"

import { useState, useRef, useCallback, useEffect } from "react"
import { fetchMessageTimeline } from "@/lib/api"
import { mapApiMessage } from "@/lib/mappers"
import { isLiveSegmentCoveredByDurable } from "@/lib/messages"
import type { Message } from "@/lib/types"

// Timeline page size — durable turns + activity_summary rows (not live tool floods).
const PAGE_SIZE = 100
const ROLE_ORDER: Record<Message["role"], number> = {
  user: 0,
  hub: 1,
  claw: 2,
  activity: 3,
  activity_summary: 4,
  system: 5,
}

function conversationMessages(messages: Message[]): Message[] {
  // Cursor pagination is keyed off real conversation rows, not activity chrome.
  return messages.filter((message) => message.role !== "activity" && message.role !== "activity_summary")
}

function oldestConversationCursor(messages: Message[]): string | null {
  const oldest = conversationMessages(messages)[0]
  if (!oldest) return null
  return oldest.timestamp instanceof Date ? oldest.timestamp.toISOString() : String(oldest.timestamp)
}

function hasOlderConversationPage(messages: Message[], pageSize: number): boolean {
  const conversations = conversationMessages(messages)
  if (conversations.length < pageSize) return false
  return messages[0]?.role !== "activity_summary"
}

interface UseWindowedMessagesOptions {
  clawId: string
  liveMessages: Message[] // streaming/new messages from websocket
}

interface UseWindowedMessages {
  messages: Message[]
  hasOlder: boolean
  loadingOlder: boolean
  loadOlder: () => Promise<void>
  scrollRef: React.RefObject<HTMLDivElement | null>
  onScroll: () => void
}

export function useWindowedMessages({ clawId, liveMessages }: UseWindowedMessagesOptions): UseWindowedMessages {
  const [historicalMsgs, setHistoricalMsgs] = useState<Message[]>([])
  const [hasOlder, setHasOlder] = useState(false)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const loadedClawId = useRef<string | null>(null)
  const oldestTimestamp = useRef<string | null>(null)

  // Initial load — last PAGE_SIZE messages
  useEffect(() => {
    if (!clawId || loadedClawId.current === clawId) return
    loadedClawId.current = clawId
    oldestTimestamp.current = null
    setHistoricalMsgs([])
    setHasOlder(false)

    fetchMessageTimeline(clawId, { limit: PAGE_SIZE })
      .then((apiMsgs) => {
        const msgs = apiMsgs.map(mapApiMessage)
        setHistoricalMsgs(msgs)
        oldestTimestamp.current = oldestConversationCursor(msgs)
        setHasOlder(hasOlderConversationPage(msgs, PAGE_SIZE))
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
      const apiMsgs = await fetchMessageTimeline(clawId, { before: oldestTimestamp.current, limit: PAGE_SIZE })
      const older = apiMsgs.map(mapApiMessage)

      if (older.length === 0) {
        setHasOlder(false)
        return
      }

      setHasOlder(hasOlderConversationPage(older, PAGE_SIZE))
      oldestTimestamp.current = oldestConversationCursor(older)

      setHistoricalMsgs((prev) => [...older, ...prev])

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

  // Merge historical + live, deduplicate, sort by timestamp, cap window
  const messages = (() => {
    const seen = new Set<string>()
    const all: Message[] = []
    for (const m of historicalMsgs) {
      if (!seen.has(m.id)) { seen.add(m.id); all.push(m) }
    }
    // Claim durable ids so repeated assistant text across turns does not
    // hide a new live segment that happens to match older prose.
    const claimedDurableIds = new Set<string>()
    for (const m of liveMessages) {
      if (seen.has(m.id)) continue
      if (all.some((existing) => isDuplicateLiveActivity(existing, m))) continue
      // Drop live only when a nearby durable row is this segment's flush —
      // not every historical message with identical content.
      if (isLiveSegmentCoveredByDurable(m, all, claimedDurableIds)) continue
      seen.add(m.id)
      all.push(m)
    }
    all.sort(compareMessages)
    return all
  })()

  return { messages, hasOlder, loadingOlder, loadOlder, scrollRef, onScroll }
}

function compareMessages(a: Message, b: Message): number {
  const timeDelta = a.timestamp.getTime() - b.timestamp.getTime()
  if (timeDelta !== 0) return timeDelta
  return (ROLE_ORDER[a.role] ?? 9) - (ROLE_ORDER[b.role] ?? 9)
}

function isDuplicateLiveActivity(existing: Message, candidate: Message): boolean {
  if (existing.role !== "activity" || candidate.role !== "activity") return false
  const timeDelta = Math.abs(existing.timestamp.getTime() - candidate.timestamp.getTime())
  if (timeDelta > 2000) return false
  if (existing.content !== candidate.content) return false

  const existingActivity = existing.activity
  const candidateActivity = candidate.activity
  if (!existingActivity || !candidateActivity) return true

  return (
    existingActivity.kind === candidateActivity.kind &&
    existingActivity.phase === candidateActivity.phase &&
    existingActivity.tool === candidateActivity.tool &&
    existingActivity.command === candidateActivity.command &&
    existingActivity.path === candidateActivity.path &&
    existingActivity.url === candidateActivity.url
  )
}
