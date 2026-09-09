"use client"

import { useState, useRef, useCallback, useEffect, useLayoutEffect, useMemo } from "react"
import { fetchMessageTimeline } from "@/lib/api"
import { mapApiMessage } from "@/lib/mappers"
import { expandSummaryMessage, selectTrailingSummaries } from "@/lib/activity-prefetch"
import { isDuplicateLiveActivity, isLiveSegmentCoveredByDurable } from "@/lib/messages"
import { useProgrammaticScrollFlag } from "@/hooks/use-pinned-scroll"
import type { Message } from "@/lib/types"

// Timeline page size — durable turns + activity_summary rows (not live tool floods).
const PAGE_SIZE = 100
// The timeline endpoint returns activity_summary placeholders, not the tool
// rows themselves — so a freshly loaded transcript would report "done" on a
// turn whose last step is still running and "no failures" over failures it
// simply has not loaded. Prefetch the trailing summaries (enough to cover the
// current/last turn) so the loaded state is honest without manual expansion.
const PREFETCH_SUMMARY_COUNT = 2
const PREFETCH_ROW_BUDGET = 200
// Per-summary cap when the user explicitly asks to load all remaining calls.
const LOAD_ALL_SUMMARY_LIMIT = 500
// After a prepend restores the reading position, ignore top-of-scroll pager
// triggers briefly — late layout (markdown, images) can shift content back
// under the threshold and would otherwise page again immediately.
const LOAD_OLDER_COOLDOWN_MS = 600
const ROLE_ORDER: Record<Message["role"], number> = {
  user: 0,
  hub: 1,
  claw: 2,
  activity: 3,
  activity_summary: 4,
  system: 5,
  state: 6,
}

function conversationMessages(messages: Message[]): Message[] {
  // Cursor pagination is keyed off real conversation rows, not activity/state chrome.
  return messages.filter((message) => message.role !== "activity" && message.role !== "activity_summary" && message.role !== "state")
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
  /** True while a scroll we initiated is settling — scroll handlers must ignore those events. */
  isProgrammaticScrollRef: React.RefObject<boolean>
  /** Call right before any programmatic `scrollTop` write on `scrollRef`. */
  markProgrammaticScroll: () => void
  /** Tool calls still hidden behind unexpanded activity_summary rows. */
  unloadedActivityCount: number
  loadingActivity: boolean
  /** Expand every remaining activity_summary into real rows (explicit user action). */
  loadAllActivity: () => Promise<void>
}

export function useWindowedMessages({ clawId, liveMessages }: UseWindowedMessagesOptions): UseWindowedMessages {
  const [historicalMsgs, setHistoricalMsgs] = useState<Message[]>([])
  const [hasOlder, setHasOlder] = useState(false)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const loadedClawId = useRef<string | null>(null)
  const oldestTimestamp = useRef<string | null>(null)
  const { isProgrammaticRef: isProgrammaticScrollRef, mark: markProgrammaticScroll } = useProgrammaticScrollFlag()
  const loadInFlight = useRef(false)
  const cooldownUntil = useRef(0)
  // scrollHeight captured immediately before a prepend commits; consumed by the
  // restore layout effect below.
  const pendingRestore = useRef<number | null>(null)
  // Bumped on every claw switch; fetches capture it at start and drop their
  // response if it moved — otherwise agent A's slow initial or "load older"
  // fetch would land after switching to agent B and replace B's transcript.
  const fetchGeneration = useRef(0)
  const [loadingActivity, setLoadingActivity] = useState(false)
  const loadAllInFlight = useRef(false)
  // Mirror so loadAllActivity sees the current transcript without re-creating
  // the callback on every message.
  const historicalRef = useRef<Message[]>([])
  useEffect(() => {
    historicalRef.current = historicalMsgs
  }, [historicalMsgs])

  // Initial load — last PAGE_SIZE messages
  useEffect(() => {
    if (!clawId || loadedClawId.current === clawId) return
    loadedClawId.current = clawId
    fetchGeneration.current += 1
    const generation = fetchGeneration.current
    oldestTimestamp.current = null
    pendingRestore.current = null
    cooldownUntil.current = 0
    setHistoricalMsgs([])
    setHasOlder(false)

    fetchMessageTimeline(clawId, { limit: PAGE_SIZE })
      .then(async (apiMsgs) => {
        if (fetchGeneration.current !== generation) return
        const msgs = apiMsgs.map(mapApiMessage)
        setHistoricalMsgs(msgs)
        oldestTimestamp.current = oldestConversationCursor(msgs)
        setHasOlder(hasOlderConversationPage(msgs, PAGE_SIZE))

        // Prefetch the trailing summaries so the last turn's steps are real on
        // load: without them the Problems filter, the now strip and the turn
        // status pill are all blind to what actually happened.
        for (const pick of selectTrailingSummaries(msgs, {
          maxSummaries: PREFETCH_SUMMARY_COUNT,
          rowBudget: PREFETCH_ROW_BUDGET,
        })) {
          const target = msgs.find((m) => m.id === pick.id)
          if (!target) continue
          try {
            const replacement = await expandSummaryMessage(clawId, target, pick.limit)
            if (fetchGeneration.current !== generation) return
            setHistoricalMsgs((prev) => prev.flatMap((m) => (m.id === pick.id ? replacement : [m])))
          } catch (err) {
            console.warn("Failed to prefetch activity rows:", err)
          }
        }
      })
      .catch(console.warn)
  }, [clawId])

  // Load every remaining summary — the "Load them" affordance of the Problems
  // filter. Sequential on purpose: bounded, cancellable via the generation.
  const loadAllActivity = useCallback(async () => {
    if (loadAllInFlight.current) return
    loadAllInFlight.current = true
    setLoadingActivity(true)
    const generation = fetchGeneration.current
    try {
      const summaries = historicalRef.current.filter(
        (m) => m.role === "activity_summary" && m.activitySummary
      )
      for (const summary of summaries) {
        const replacement = await expandSummaryMessage(clawId, summary, LOAD_ALL_SUMMARY_LIMIT)
        if (fetchGeneration.current !== generation) return
        setHistoricalMsgs((prev) => prev.flatMap((m) => (m.id === summary.id ? replacement : [m])))
      }
    } catch (err) {
      console.warn("Failed to load tool calls:", err)
    } finally {
      loadAllInFlight.current = false
      setLoadingActivity(false)
    }
  }, [clawId])

  const unloadedActivityCount = useMemo(
    () =>
      historicalMsgs.reduce(
        (sum, m) => sum + (m.role === "activity_summary" ? m.activitySummary?.count ?? 0 : 0),
        0
      ),
    [historicalMsgs]
  )

  // Load older messages when user scrolls to top
  const loadOlder = useCallback(async () => {
    if (!hasOlder || loadInFlight.current || !oldestTimestamp.current) return
    loadInFlight.current = true
    setLoadingOlder(true)
    const generation = fetchGeneration.current

    try {
      const apiMsgs = await fetchMessageTimeline(clawId, { before: oldestTimestamp.current, limit: PAGE_SIZE })
      if (fetchGeneration.current !== generation) return
      const older = apiMsgs.map(mapApiMessage)

      if (older.length === 0) {
        setHasOlder(false)
        return
      }

      setHasOlder(hasOlderConversationPage(older, PAGE_SIZE))
      oldestTimestamp.current = oldestConversationCursor(older)

      // Capture the height right before the prepend commits (not before the
      // fetch — streaming may have grown the transcript while it was in
      // flight). The restore effect below turns it into a delta once the new
      // rows have actually laid out.
      pendingRestore.current = scrollRef.current?.scrollHeight ?? 0
      setHistoricalMsgs((prev) => [...older, ...prev])
    } catch (err) {
      console.warn("Failed to load older messages:", err)
    } finally {
      loadInFlight.current = false
      setLoadingOlder(false)
    }
  }, [clawId, hasOlder])

  // Restore the reading position after a prepend. A layout effect keyed on the
  // first message id runs synchronously after the new rows render, so the
  // scrollHeight delta is measured against the real post-prepend layout — a
  // single pre-captured height restored in a rAF could land mid-layout, jump
  // the view, and immediately retrigger the top-of-scroll pager.
  const firstHistoricalId = historicalMsgs[0]?.id
  useLayoutEffect(() => {
    if (pendingRestore.current === null) return
    const prevHeight = pendingRestore.current
    pendingRestore.current = null
    const el = scrollRef.current
    if (el) {
      markProgrammaticScroll()
      el.scrollTop += el.scrollHeight - prevHeight
    }
    cooldownUntil.current = Date.now() + LOAD_OLDER_COOLDOWN_MS
  }, [firstHistoricalId, markProgrammaticScroll])

  // Trigger loadOlder when the user scrolls near the top
  const onScroll = useCallback(() => {
    if (isProgrammaticScrollRef.current) return
    const el = scrollRef.current
    if (!el || loadInFlight.current || !hasOlder) return
    if (Date.now() < cooldownUntil.current) return
    if (el.scrollTop < 200) {
      loadOlder()
    }
  }, [loadOlder, hasOlder, isProgrammaticScrollRef])

  // Merge historical + live, deduplicate, sort by timestamp, cap window.
  // Memoized — this array drives the auto-scroll effects downstream, so a
  // fresh identity on every render would re-run them during streaming.
  const messages = useMemo(() => {
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
  }, [historicalMsgs, liveMessages])

  return {
    messages,
    hasOlder,
    loadingOlder,
    loadOlder,
    scrollRef,
    onScroll,
    isProgrammaticScrollRef,
    markProgrammaticScroll,
    unloadedActivityCount,
    loadingActivity,
    loadAllActivity,
  }
}

function compareMessages(a: Message, b: Message): number {
  const timeDelta = a.timestamp.getTime() - b.timestamp.getTime()
  if (timeDelta !== 0) return timeDelta
  return (ROLE_ORDER[a.role] ?? 9) - (ROLE_ORDER[b.role] ?? 9)
}

