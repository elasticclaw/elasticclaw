// Shared trailing-summary prefetch logic. The timeline endpoint returns
// activity_summary placeholders instead of tool rows, so both the chat view
// (use-windowed-messages) and the board (use-board-activity-prefetch) need the
// same "pick the trailing summaries and expand them into real rows" behavior.

import { fetchActivityMessages } from "@/lib/api"
import { mapApiMessage } from "@/lib/mappers"
import { isPrunedActivitySummary } from "@/lib/messages"
import type { Message } from "@/lib/types"

export interface SummaryPick {
  id: string
  limit: number
}

/**
 * Newest-first picks of unexpanded summaries, bounded by count and row budget.
 *
 * Client-side pruned markers are never picked: they stand for rows the live
 * cache just evicted to stay under its cap, so expanding them re-inserts those
 * rows, trips the cap again, and emits a fresh range-scoped marker id the
 * caller has never seen — one hub fetch per activity event, forever. They stay
 * collapsed (still counted, still honest) instead.
 */
export function selectTrailingSummaries(
  messages: Message[],
  opts: { maxSummaries: number; rowBudget: number }
): SummaryPick[] {
  const picks: SummaryPick[] = []
  let budget = opts.rowBudget
  for (let i = messages.length - 1; i >= 0 && picks.length < opts.maxSummaries && budget > 0; i -= 1) {
    const message = messages[i]
    if (message.role !== "activity_summary" || !message.activitySummary) continue
    if (isPrunedActivitySummary(message)) continue
    const count = message.activitySummary.count || 0
    if (count <= 0) continue
    const limit = Math.min(Math.max(1, count), budget)
    picks.push({ id: message.id, limit })
    budget -= limit
  }
  return picks
}

/**
 * Fetch the activity rows behind one activity_summary placeholder and return
 * the messages that should replace it in the transcript. When the summary is
 * larger than `limit`, the newest rows are loaded and the unloaded remainder
 * stays behind as a smaller summary — still visible and expandable, never
 * silently dropped.
 *
 * `keepEmptyRemainder` keeps a zero-count remainder row (same id) even when
 * everything loaded. The board needs it: summary ids are deterministic per
 * range server-side, so a surviving row lets later timeline merges recognize
 * the range as already expanded instead of re-adding the full placeholder.
 */
export async function expandSummaryMessage(
  clawId: string,
  summary: Message,
  limit: number,
  opts?: { keepEmptyRemainder?: boolean }
): Promise<Message[]> {
  const meta = summary.activitySummary
  if (!meta) return [summary]
  const count = meta.count || 0
  const capped = Math.min(Math.max(1, count), limit)
  const newestFirst = count > capped
  const apiMsgs = await fetchActivityMessages(clawId, {
    from: meta.from,
    to: meta.to,
    limit: capped,
    order: newestFirst ? "desc" : "asc",
  })
  const rows = apiMsgs.map(mapApiMessage)
  if (newestFirst) rows.reverse()
  if (rows.length === 0) return [summary]
  if (newestFirst) {
    const remainder: Message = {
      ...summary,
      activitySummary: {
        count: count - rows.length,
        from: meta.from,
        // Exclusive of the oldest loaded row so a later expand does not refetch it.
        to: new Date(rows[0].timestamp.getTime() - 1).toISOString(),
      },
    }
    return [remainder, ...rows]
  }
  if (opts?.keepEmptyRemainder) {
    const remainder: Message = {
      ...summary,
      activitySummary: {
        count: Math.max(0, count - rows.length),
        from: meta.from,
        to: meta.to,
      },
    }
    return [remainder, ...rows]
  }
  return rows
}
