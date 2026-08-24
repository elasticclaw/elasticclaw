import type { Message } from "@/lib/types"

export function isTerminalAssistantMessage(message: Message): boolean {
  return message.role === "claw" && /\[(DONE|READY_TO_COMMIT)\]/.test(message.content)
}

/** Client-only rows that never come from the timeline API. */
export function isTransientMessage(message: Message): boolean {
  // Server-issued summary rows have ids like "activity-summary-<uuid>" which
  // collide with the client "activity-" prefix — they are durable API rows
  // (their cached remainder must survive merges), never transient.
  if (message.role === "activity_summary") return false
  return (
    message.id.startsWith("activity-") ||
    message.id.startsWith("live-") ||
    message.id.startsWith("thinking-")
  )
}

/** How long after a live segment a durable flush may still count as its twin. */
export const LIVE_SEGMENT_DURABLE_MATCH_MS = 60_000

function messageTimeMs(message: Message): number {
  return message.timestamp instanceof Date
    ? message.timestamp.getTime()
    : new Date(message.timestamp).getTime()
}

/**
 * True when a client live-* segment is already represented by a durable
 * timeline/API message (hub flush or promoted final).
 *
 * A durable row covers a live segment only when:
 * - same role + content
 * - durable time is in [live, live + match window] — never *before* live, so
 *   a preceding durable with the same prose cannot hide a new segment
 * - each durable id is claimed at most once when `claimedDurableIds` is set
 */
export function isLiveSegmentCoveredByDurable(
  live: Message,
  durableCandidates: Message[],
  claimedDurableIds?: Set<string>,
  maxDeltaMs: number = LIVE_SEGMENT_DURABLE_MATCH_MS
): boolean {
  if (!live.id.startsWith("live-") || !live.content.trim()) return false
  const liveTime = messageTimeMs(live)
  const latest = liveTime + maxDeltaMs
  let bestId: string | null = null
  let bestDelta = Infinity
  for (const durable of durableCandidates) {
    if (isTransientMessage(durable)) continue
    if (durable.role === "activity" || durable.role === "activity_summary") continue
    if (claimedDurableIds?.has(durable.id)) continue
    if (durable.role !== live.role || durable.content !== live.content) continue
    const durableTime = messageTimeMs(durable)
    // Strictly no earlier durable: preceding same-text rows are prior turns.
    // Equal timestamps OK (same flush event / rounded ms).
    if (durableTime < liveTime || durableTime > latest) continue
    const delta = durableTime - liveTime
    if (delta < bestDelta) {
      bestDelta = delta
      bestId = durable.id
    }
  }
  if (!bestId) return false
  claimedDurableIds?.add(bestId)
  return true
}

/**
 * True when a client live activity row and a durable activity row describe the
 * same tool event (same content and activity identity, close in time). Used to
 * drop the transient twin once the durable row is loaded.
 */
export function isDuplicateLiveActivity(existing: Message, candidate: Message): boolean {
  if (existing.role !== "activity" || candidate.role !== "activity") return false
  const timeDelta = Math.abs(messageTimeMs(existing) - messageTimeMs(candidate))
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

/**
 * Durable conversation rows that must not be aged out by a flood of live
 * tool/activity events. Includes timeline activity_summary placeholders.
 */
export function isDurableConversationMessage(message: Message): boolean {
  if (isTransientMessage(message)) return false
  if (message.role === "activity") return false
  return true
}

/**
 * Window a transcript for compact surfaces (board cards) by durable turns,
 * not by raw row count. Activities and live segments never push durable
 * user/claw messages out of the window.
 *
 * Keeps the last `durableLimit` durable messages and every row from the
 * first kept durable message onward (so in-window tool activity stays).
 */
export function windowMessagesByDurableCount(
  messages: Message[],
  durableLimit: number
): Message[] {
  if (durableLimit <= 0 || messages.length === 0) return messages

  let durableCount = 0
  let startIdx = 0
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (!isDurableConversationMessage(messages[i])) continue
    durableCount += 1
    if (durableCount >= durableLimit) {
      startIdx = i
      break
    }
  }
  return messages.slice(startIdx)
}

/** Id of the placeholder standing in for locally pruned activity rows. */
export const PRUNED_ACTIVITY_SUMMARY_ID = "activity-summary-pruned"

/**
 * Drop the oldest live activity rows once they exceed `maxActivities`,
 * without touching durable conversation messages or live text segments.
 *
 * The removed rows leave an `activity_summary` placeholder behind, covering
 * their time range so it can be expanded from the API like any other summary.
 * Deleting them outright made the window look complete while it was not:
 * counts derived from it (the sidebar's "N of M subagents") would undercount a
 * long turn and present the undercount as fact. One marker is kept — an
 * earlier one is folded into the new one.
 */
export function pruneOldestLiveActivities(
  messages: Message[],
  maxActivities: number
): Message[] {
  if (maxActivities < 0 || messages.length === 0) return messages

  let activityCount = 0
  for (const message of messages) {
    if (message.role === "activity") activityCount += 1
  }
  if (activityCount <= maxActivities) return messages

  let toDrop = activityCount - maxActivities
  let dropped = 0
  let fromMs = Infinity
  let toMs = -Infinity
  // Position of the *last* row folded into the marker, not the first: the
  // marker is stamped with `toMs`, the newest time it covers, and the two must
  // agree. Placed at the first dropped row instead, a marker that absorbed an
  // earlier one could sit before the current turn's user message while
  // covering activity rows from inside that turn — and currentTurnSubagents,
  // which scans only from that user message on, would then miss it and publish
  // an undercount as fact.
  let insertAt = -1
  const kept: Message[] = []

  const cover = (start: number, end: number) => {
    if (Number.isFinite(start)) fromMs = Math.min(fromMs, start)
    if (Number.isFinite(end)) toMs = Math.max(toMs, end)
  }

  for (const message of messages) {
    if (message.id === PRUNED_ACTIVITY_SUMMARY_ID) {
      dropped += message.activitySummary?.count ?? 0
      const meta = message.activitySummary
      cover(meta?.from ? Date.parse(meta.from) : messageTimeMs(message), meta?.to ? Date.parse(meta.to) : messageTimeMs(message))
      insertAt = kept.length
      continue
    }
    if (message.role === "activity" && toDrop > 0) {
      toDrop -= 1
      dropped += 1
      cover(messageTimeMs(message), messageTimeMs(message))
      insertAt = kept.length
      continue
    }
    kept.push(message)
  }

  if (dropped <= 0 || insertAt === -1) return kept

  const marker: Message = {
    id: PRUNED_ACTIVITY_SUMMARY_ID,
    role: "activity_summary",
    content: "",
    timestamp: new Date(toMs),
    activitySummary: {
      count: dropped,
      from: new Date(fromMs).toISOString(),
      to: new Date(toMs).toISOString(),
    },
  }
  kept.splice(insertAt, 0, marker)
  return kept
}
