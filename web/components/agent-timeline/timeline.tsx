"use client"

import { useCallback, useMemo, useState, type ReactNode } from "react"
import type { Message } from "@/lib/types"
import { groupIntoTurns, type Turn } from "@/lib/turns"
import { useNowTick } from "@/hooks/use-now"
import { TurnCard } from "./turn-card"
import { ToggleAnchorContext } from "./anchor-context"
import type { TimelineDensity } from "./timeline-toolbar"

function turnDefaultExpanded(turn: Turn, isLast: boolean, density: TimelineDensity): boolean {
  if (density === "all" || density === "tools") return true
  if (density === "problems") return turn.hasProblems || turn.failedCount > 0
  return isLast
}

/**
 * The turn-based transcript. Owns per-turn expansion (current turn expanded,
 * older turns collapsed) and the scroll anchoring that keeps the reading
 * position still when anything above the viewport expands or collapses.
 */
export function AgentTimeline({
  clawId,
  messages,
  density,
  renderMessage,
  isWorking,
  streamingSlot,
  scrollRef,
  pinnedRef,
  markProgrammaticScroll,
}: {
  clawId: string
  messages: Message[]
  density: TimelineDensity
  renderMessage: (message: Message) => ReactNode
  /** Claw is streaming or mid-tool — the last turn presents as running. */
  isWorking: boolean
  /** Live streaming prose, rendered at the tail of the last turn. */
  streamingSlot?: ReactNode
  scrollRef: React.RefObject<HTMLElement | null>
  /** While pinned to bottom the auto-scroll owns the position — skip anchoring. */
  pinnedRef?: React.RefObject<boolean>
  markProgrammaticScroll?: () => void
}) {
  const turns = useMemo(() => groupIntoTurns(messages), [messages])
  const [expandedOverrides, setExpandedOverrides] = useState<Record<string, boolean>>({})
  const hasLiveWork = isWorking || turns.some((t) => t.status === "running")
  const now = useNowTick(hasLiveWork)

  // Compensate scrollTop after a toggle so content above the viewport does not
  // shift the reading position. Discrete-event state updates flush
  // synchronously in React DOM, so by the time the microtask runs the DOM has
  // the post-toggle layout.
  const anchor = useCallback(
    (el: HTMLElement) => {
      const scroller = scrollRef.current
      if (!scroller || pinnedRef?.current) return
      const top = el.getBoundingClientRect().top
      queueMicrotask(() => {
        const delta = el.getBoundingClientRect().top - top
        if (delta !== 0) {
          markProgrammaticScroll?.()
          scroller.scrollTop += delta
        }
      })
    },
    [scrollRef, pinnedRef, markProgrammaticScroll]
  )

  const visibleTurns = density === "problems"
    ? turns.filter((turn, i) => turn.hasProblems || turn.failedCount > 0 || (i === turns.length - 1 && isWorking))
    : turns

  if (density === "problems" && visibleTurns.length === 0) {
    return (
      <p className="py-12 text-center text-sm text-muted-foreground">
        No failures in this transcript.
      </p>
    )
  }

  return (
    <ToggleAnchorContext.Provider value={anchor}>
      <div className="space-y-3">
        {visibleTurns.map((turn) => {
          const isLast = turn === turns[turns.length - 1]
          const expanded =
            expandedOverrides[`${density}:${turn.id}`] ?? turnDefaultExpanded(turn, isLast, density)
          return (
            <TurnCard
              key={turn.id}
              turn={turn}
              density={density}
              expanded={expanded}
              onToggle={() =>
                setExpandedOverrides((prev) => ({ ...prev, [`${density}:${turn.id}`]: !expanded }))
              }
              clawId={clawId}
              renderMessage={renderMessage}
              now={hasLiveWork && isLast ? now : undefined}
              forceRunning={isLast && isWorking}
            >
              {isLast && (density === "conversation" || density === "all") ? streamingSlot : null}
            </TurnCard>
          )
        })}
        {turns.length === 0 && (density === "conversation" || density === "all") && streamingSlot}
      </div>
    </ToggleAnchorContext.Provider>
  )
}
