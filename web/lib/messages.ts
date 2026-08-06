import type { Message } from "@/lib/types"

export function isTerminalAssistantMessage(message: Message): boolean {
  return message.role === "claw" && /\[(DONE|READY_TO_COMMIT)\]/.test(message.content)
}

/** Client-only rows that never come from the timeline API. */
export function isTransientMessage(message: Message): boolean {
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

/**
 * Drop the oldest live activity rows once they exceed `maxActivities`,
 * without touching durable conversation messages or live text segments.
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
  return messages.filter((message) => {
    if (message.role !== "activity" || toDrop <= 0) return true
    toDrop -= 1
    return false
  })
}
