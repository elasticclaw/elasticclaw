import type { Message } from "@/lib/types"

export function isTerminalAssistantMessage(message: Message): boolean {
  return message.role === "claw" && /\[(DONE|READY_TO_COMMIT)\]/.test(message.content)
}

// Transient messages are synthesized client-side (activity ticker, live
// streaming, thinking indicator) and must never survive a timeline merge or
// cache persistence.
export function isTransientMessage(message: Message): boolean {
  return message.id.startsWith("activity-") || message.id.startsWith("live-") || message.id.startsWith("thinking-")
}

// Optimistic messages are appended by send() with a local "opt-" id and are
// swapped for the server-issued UUID once the API confirms them.
export function isOptimisticMessage(message: Message): boolean {
  return message.id.startsWith("opt-")
}

/**
 * Merge an API timeline fetch with the locally cached/optimistic state:
 * 1. Keep cached durable messages missing from the API result (the cache can
 *    extend beyond the API fetch window).
 * 2. Re-append in-flight optimistic messages that the API has not confirmed
 *    yet (same role + content) so send() can still swap them with real UUIDs.
 * 3. Drop transient messages; they are re-synthesized from live events.
 * The result is sorted by timestamp (stable sort, so equal timestamps keep
 * API-first ordering).
 */
export function mergeTimelineMessages(existing: Message[], incoming: Message[]): Message[] {
  const incomingIds = new Set(incoming.map((m) => m.id))
  const cachedOnly = existing.filter(
    (m) => !isOptimisticMessage(m) && !isTransientMessage(m) && !incomingIds.has(m.id)
  )
  const inflight = existing.filter(
    (m) => isOptimisticMessage(m) && !incoming.some((r) => r.content === m.content && r.role === m.role)
  )
  const merged = [...incoming, ...cachedOnly, ...inflight]
  merged.sort((a, b) => a.timestamp.getTime() - b.timestamp.getTime())
  return merged
}
