"use client"

import { useEffect, useRef } from "react"
import type { Claw, Message } from "@/lib/types"

// Cap concurrent claw prefetches so a board full of agents does not stampede
// the hub right after the initial timeline loads.
const MAX_CONCURRENT_PREFETCHES = 3
/** Offline claws seen within this window still get step rows prefetched. */
const OFFLINE_PREFETCH_WINDOW_MS = 60 * 60 * 1000

function isWorthPrefetching(claw: Claw): boolean {
  if (claw.isStreaming) return true
  switch (claw.status) {
    case "connected":
    case "idle":
    case "error":
      return true
    case "offline": {
      // Skip long-offline claws; a recently dropped one is still interesting.
      if (!claw.last_seen) return false
      const seen = new Date(claw.last_seen).getTime()
      return Number.isFinite(seen) && Date.now() - seen < OFFLINE_PREFETCH_WINDOW_MS
    }
    default:
      return false
  }
}

/**
 * Board-side companion of the chat view's trailing-summary prefetch: every
 * card must show real step rows without user interaction, so once a claw's
 * timeline lands in hub state, expand its trailing activity_summary rows.
 *
 * Keyed on summary ids, not claws: the cached transcript renders before the
 * timeline fetch returns its placeholders, so a claw must be re-considered
 * when new summary rows appear — but each summary id is only processed once
 * (the prefetch may intentionally leave a large one partially expanded, and
 * its remainder keeps the id).
 *
 * Bounded (the per-claw budget lives in use-hub), capped concurrency, and
 * cancelled via the generation guard when the board unmounts or the claw list
 * changes — interrupted work is retried on the next activation.
 */
export function useBoardActivityPrefetch({
  active,
  claws,
  messages,
  prefetch,
}: {
  /** True while the board (no claw selected) is the visible view. */
  active: boolean
  claws: Claw[]
  messages: Record<string, Message[]>
  /** From use-hub: expands the claw's trailing summaries; false = interrupted. */
  prefetch: (clawId: string, cancelled?: () => boolean) => Promise<boolean>
}) {
  // Summary ids already claimed by a queued/completed prefetch.
  const processedRef = useRef(new Set<string>())
  const generationRef = useRef(0)
  const queueRef = useRef<{ clawId: string; summaryIds: string[] }[]>([])
  const inFlightRef = useRef(0)

  // Cancellation boundary: board hidden or the claw set itself changed.
  const clawSetKey = claws
    .map((c) => c.id)
    .sort()
    .join(",")
  useEffect(() => {
    // The Set/array instances are stable for the hook's lifetime; captured so
    // the cleanup does not read .current (react-hooks/exhaustive-deps).
    const processed = processedRef.current
    const queue = queueRef
    return () => {
      generationRef.current += 1
      for (const job of queue.current) {
        for (const id of job.summaryIds) processed.delete(id)
      }
      queue.current = []
    }
  }, [active, clawSetKey])

  useEffect(() => {
    if (!active) return
    const generation = generationRef.current
    const cancelled = () => generationRef.current !== generation

    for (const claw of claws) {
      if (!isWorthPrefetching(claw)) continue
      const msgs = messages[claw.id]
      if (!msgs || msgs.length === 0) continue
      const summaryIds = msgs
        .filter(
          (m) =>
            m.role === "activity_summary" &&
            (m.activitySummary?.count ?? 0) > 0 &&
            !processedRef.current.has(m.id)
        )
        .map((m) => m.id)
      if (summaryIds.length === 0) continue
      // Claim every current summary id — the prefetch only expands the
      // trailing ones (bounded), and the older placeholders stay collapsed
      // by design rather than being retried forever.
      for (const id of summaryIds) processedRef.current.add(id)
      queueRef.current.push({ clawId: claw.id, summaryIds })
    }

    const pump = () => {
      if (cancelled()) return
      while (inFlightRef.current < MAX_CONCURRENT_PREFETCHES && queueRef.current.length > 0) {
        const job = queueRef.current.shift()
        if (!job) break
        inFlightRef.current += 1
        prefetch(job.clawId, cancelled)
          .then((completed) => {
            if (!completed) {
              for (const id of job.summaryIds) processedRef.current.delete(id)
            }
          })
          .catch(() => {
            for (const id of job.summaryIds) processedRef.current.delete(id)
          })
          .finally(() => {
            inFlightRef.current -= 1
            pump()
          })
      }
    }
    pump()
  }, [active, claws, messages, prefetch])
}
