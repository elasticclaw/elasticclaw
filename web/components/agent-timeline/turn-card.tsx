"use client"

import { memo, useMemo } from "react"
import { ChevronDown, ChevronRight } from "lucide-react"
import { cn } from "@/lib/utils"
import {
  collapseStepRuns,
  formatDurationMs,
  isProblemStep,
  turnLabel,
  type Step,
  type Turn,
  type TurnStatus,
} from "@/lib/turns"
import { StepList } from "./step-row"
import { ActivitySummaryBlock } from "./activity-summary-block"
import { useToggleAnchor } from "./anchor-context"
import type { TimelineDensity } from "./timeline-toolbar"

function formatClock(date: Date): string {
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
}

/** Filters + collapses step runs, memoized — this math should not re-run for
 *  every streaming frame of an unrelated part of the transcript. */
const CollapsedStepList = memo(function CollapsedStepList({
  steps,
  problemsOnly,
  now,
  onOpenSubagent,
}: {
  steps: Step[]
  problemsOnly: boolean
  now?: number
  onOpenSubagent?: (stepId: string) => void
}) {
  const items = useMemo(() => {
    const visible = problemsOnly ? steps.filter(isProblemStep) : steps
    return collapseStepRuns(visible)
  }, [steps, problemsOnly])
  if (items.length === 0) return null
  return <StepList items={items} now={now} onOpenSubagent={onOpenSubagent} />
})

/**
 * The step-work portion of a turn: a hairline "fold" header ("Edited hub ·
 * Worked for 2m 14s · 12 tool calls") with the step rows beneath it. No card:
 * the fold's bottom hairline is the only separation between turns.
 * Conversation bubbles are deliberately rendered by AgentTimeline so they
 * remain chronologically independent from the work.
 *
 * Memoized: during streaming the owner re-renders per frame, but only the
 * last turn's props actually change — older folds must not recompute labels
 * and step grouping 60 times a second.
 */
export const TurnCard = memo(function TurnCard({
  turn,
  density,
  expanded,
  toggleKey,
  onToggle,
  clawId,
  now,
  forceRunning,
  onOpenSubagent,
}: {
  turn: Turn
  density: TimelineDensity
  /** Whether the step rows are expanded (the turn chevron state). */
  expanded: boolean
  /** Identity of this fold's expansion state in the owner's override map. */
  toggleKey: string
  onToggle: (key: string, expanded: boolean) => void
  clawId: string
  /** Live clock while this turn has running steps. */
  now?: number
  /** Claw is streaming — the last turn shows as running even between steps. */
  forceRunning?: boolean
  /** Opt-in Task-step drill-down; see StepRow. */
  onOpenSubagent?: (stepId: string) => void
}) {
  const anchor = useToggleAnchor()
  const problemsOnly = density === "problems"
  const status: TurnStatus = turn.status === "ok" && forceRunning ? "running" : turn.status
  const label = useMemo(() => turnLabel(turn), [turn])
  const stepNoun = `${turn.toolCallCount} tool call${turn.toolCallCount === 1 ? "" : "s"}`
  const running = status === "running"
  const elapsedMs = running && now ? Math.max(0, now - turn.startedAt.getTime()) : turn.durationMs
  const duration = elapsedMs >= 1000 ? formatDurationMs(elapsedMs) : null
  const Chevron = expanded ? ChevronDown : ChevronRight

  const renderSteps = (id: string, steps: Step[]) => (
    <CollapsedStepList key={id} steps={steps} problemsOnly={problemsOnly} now={now} onOpenSubagent={onOpenSubagent} />
  )

  return (
    <section className="flex flex-col pb-1.5">
      <div className="border-b border-border/60 pb-2 pt-1">
        <button
          type="button"
          aria-expanded={expanded}
          onClick={(e) => {
            anchor(e.currentTarget)
            onToggle(toggleKey, expanded)
          }}
          className="flex w-full min-w-0 max-w-full cursor-pointer select-none items-center gap-1 rounded-md px-1 text-left max-md:min-h-11 text-sm leading-relaxed text-muted-foreground tabular-nums transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-focus-ring"
        >
          <span className="min-w-0 truncate">{label}</span>
          <span className="shrink-0 whitespace-nowrap">
            {duration && (
              <>
                {" · "}
                <span className={cn(running && "live-tool-shine")} suppressHydrationWarning={running || undefined}>
                  {running ? "Working for " : "Worked for "}
                  {duration}
                </span>
              </>
            )}
            {!duration && running && (
              <>
                {" · "}
                <span className="live-tool-shine">Working</span>
              </>
            )}
            {turn.toolCallCount > 0 && <span> · {stepNoun}</span>}
            {turn.failedCount > 0 && <span className="text-destructive"> · {turn.failedCount} failed</span>}
          </span>
          <Chevron className="size-3.5 shrink-0" aria-hidden />
          <span className="ms-auto shrink-0 font-mono text-[.7rem] text-muted-foreground max-sm:hidden" suppressHydrationWarning>
            {formatClock(turn.startedAt)}
          </span>
        </button>
      </div>

      {expanded && (
        <div className="flex flex-col pt-1">
          {turn.items.map((item) => {
            if (item.type === "steps") {
              return expanded ? renderSteps(item.id, item.steps) : null
            }
            if (item.type === "message") return null
            // Lazy historical tool calls — keep the fetch-on-expand flow.
            if (!expanded || problemsOnly) return null
            return (
              <ActivitySummaryBlock
                key={item.message.id}
                clawId={clawId}
                summary={item.message.activitySummary}
              />
            )
          })}
        </div>
      )}
    </section>
  )
})
