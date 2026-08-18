"use client"

import { Fragment, memo, useCallback, useState, type ReactNode } from "react"
import type { Message } from "@/lib/types"
import type { Turn } from "@/lib/turns"
import { useNowTick } from "@/hooks/use-now"
import { TurnCard } from "./turn-card"
import { ToggleAnchorContext } from "./anchor-context"
import type { TimelineDensity } from "./timeline-toolbar"

function turnDefaultExpanded(turn: Turn, isLast: boolean, density: TimelineDensity): boolean {
  if (density === "all" || density === "tools") return true
  if (density === "problems") return turn.hasProblems || turn.failedCount > 0
  return isLast
}

function hasStepWork(turn: Turn): boolean {
  return turn.toolCallCount > 0 ||
    turn.items.some((item) => item.type === "summary") ||
    turn.steps.some((step) => step.status === "running")
}

const TurnMessages = memo(function TurnMessages({
  turn,
  showProse,
  firstClawMessageIndex,
  renderMessage,
  renderStepWork,
}: {
  turn: Turn
  showProse: boolean
  firstClawMessageIndex: number
  renderMessage: (message: Message) => ReactNode
  renderStepWork: () => ReactNode
}) {
  return <>
    {showProse && turn.userMessage && renderMessage(turn.userMessage)}
    {firstClawMessageIndex === -1 && renderStepWork()}
    {turn.items.map((item, index) => item.type === "message" ? (
      <Fragment key={item.message.id}>
        {showProse && renderMessage(item.message)}
        {index === firstClawMessageIndex && renderStepWork()}
      </Fragment>
    ) : null)}
  </>
})

/**
 * The Problems filter can only judge loaded tool calls — when activity_summary
 * rows remain unexpanded it must say so and offer to load them, never imply
 * the unloaded calls were clean.
 */
function UnloadedActivityNotice({
  message,
  loading,
  onLoad,
}: {
  message: string
  loading: boolean
  onLoad: () => void
}) {
  return (
    <div className="flex flex-wrap items-center justify-center gap-x-2 gap-y-1 py-4 text-center text-sm text-muted-foreground">
      <span>{message}</span>
      <button
        type="button"
        onClick={onLoad}
        disabled={loading}
        className="rounded border border-border bg-muted/30 px-2.5 py-0.5 text-xs text-foreground hover:bg-muted/50 disabled:opacity-60"
      >
        {loading ? "Loading..." : "Load them"}
      </button>
    </div>
  )
}

/**
 * The turn-based transcript. Owns per-turn expansion (current turn expanded,
 * older turns collapsed) and the scroll anchoring that keeps the reading
 * position still when anything above the viewport expands or collapses.
 */
export function AgentTimeline({
  clawId,
  turns,
  density,
  renderMessage,
  isWorking,
  streamingSlot,
  scrollRef,
  pinnedRef,
  markProgrammaticScroll,
  unloadedToolCalls = 0,
  loadingUnloaded = false,
  onLoadUnloaded,
}: {
  clawId: string
  /** Precomputed by the owner (which also derives stats/now-strip from them). */
  turns: Turn[]
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
  /** Tool calls still behind unexpanded activity_summary rows. */
  unloadedToolCalls?: number
  loadingUnloaded?: boolean
  onLoadUnloaded?: () => void
}) {
  const [expandedOverrides, setExpandedOverrides] = useState<Record<string, boolean>>({})
  const hasLiveWork = isWorking || turns.some((t) => t.status === "running")
  const now = useNowTick(hasLiveWork)

  // Compensate scrollTop after a toggle so content above the viewport does not
  // shift the reading position. Measured now, applied in a rAF: React 19
  // schedules even sync-lane renders in a microtask, so a microtask queued
  // here would run *before* the toggle commits and always measure delta 0. A
  // rAF runs after the commit but before paint, so the compensation is never
  // visible as a jump.
  const anchor = useCallback(
    (el: HTMLElement) => {
      const scroller = scrollRef.current
      if (!scroller || pinnedRef?.current) return
      const top = el.getBoundingClientRect().top
      requestAnimationFrame(() => {
        const delta = el.getBoundingClientRect().top - top
        if (delta !== 0) {
          markProgrammaticScroll?.()
          scroller.scrollTop += delta
        }
      })
    },
    [scrollRef, pinnedRef, markProgrammaticScroll]
  )

  // Stable across renders so memoized TurnCards do not re-render when a
  // sibling toggles.
  const toggleTurn = useCallback((key: string, current: boolean) => {
    setExpandedOverrides((prev) => ({ ...prev, [key]: !current }))
  }, [])

  const visibleTurns = density === "problems"
    ? turns.filter((turn, i) => turn.hasProblems || turn.failedCount > 0 || (i === turns.length - 1 && isWorking))
    : turns

  const showUnloadedNotice = density === "problems" && unloadedToolCalls > 0 && Boolean(onLoadUnloaded)
  const totalToolCalls = turns.reduce((sum, turn) => sum + turn.toolCallCount, 0)
  const loadedToolCalls = Math.max(0, totalToolCalls - unloadedToolCalls)

  if (density === "problems" && visibleTurns.length === 0) {
    if (showUnloadedNotice) {
      return (
        <UnloadedActivityNotice
          message={`No failures in the ${loadedToolCalls} loaded tool call${loadedToolCalls === 1 ? "" : "s"} · ${unloadedToolCalls} earlier call${unloadedToolCalls === 1 ? "" : "s"} not loaded`}
          loading={loadingUnloaded}
          onLoad={onLoadUnloaded!}
        />
      )
    }
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
          const toggleKey = `${density}:${turn.id}`
          const expanded = expandedOverrides[toggleKey] ?? turnDefaultExpanded(turn, isLast, density)
          const showProse = density === "conversation" || density === "all"
          const showStepWork = hasStepWork(turn)
          const firstClawMessageIndex = turn.items.findIndex(
            (item) => item.type === "message" && item.message.role === "claw"
          )
          const renderStepWork = () => showStepWork && (
            <TurnCard
              turn={turn}
              density={density}
              expanded={expanded}
              toggleKey={toggleKey}
              onToggle={toggleTurn}
              clawId={clawId}
              now={hasLiveWork && isLast ? now : undefined}
              forceRunning={isLast && isWorking}
            />
          )
          return (
            <Fragment key={turn.id}>
              <TurnMessages
                turn={turn}
                showProse={showProse}
                firstClawMessageIndex={firstClawMessageIndex}
                renderMessage={renderMessage}
                renderStepWork={renderStepWork}
              />
              {isLast && showProse ? streamingSlot : null}
            </Fragment>
          )
        })}
        {turns.length === 0 && (density === "conversation" || density === "all") && streamingSlot}
        {showUnloadedNotice && (
          <UnloadedActivityNotice
            message={`${unloadedToolCalls} earlier tool call${unloadedToolCalls === 1 ? "" : "s"} not loaded — failures in them are not shown`}
            loading={loadingUnloaded}
            onLoad={onLoadUnloaded!}
          />
        )}
      </div>
    </ToggleAnchorContext.Provider>
  )
}
