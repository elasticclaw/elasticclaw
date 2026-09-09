"use client"

import { memo, useMemo } from "react"
import { ChevronRight } from "lucide-react"
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

function StatusPill({ status }: { status: TurnStatus }) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-1.5 py-px text-[10px] font-medium",
        status === "running" && "border-blue-500/40 text-blue-400",
        status === "failed" && "border-red-500/40 text-red-400",
        status === "ok" && "border-border text-muted-foreground"
      )}
    >
      {status === "running" && (
        <span className="relative flex size-1.5">
          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-blue-400 opacity-75" />
          <span className="relative inline-flex size-1.5 rounded-full bg-blue-500" />
        </span>
      )}
      {status === "running" ? "running" : status === "failed" ? "failed" : "done"}
    </span>
  )
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
 * The step-work portion of a turn: header with generated label, status pill,
 * step count and duration; body with the step rows. Conversation bubbles are
 * deliberately rendered by AgentTimeline so they remain chronologically
 * independent from the work card.
 *
 * Memoized: during streaming the owner re-renders per frame, but only the
 * last turn's props actually change — older cards must not recompute labels
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
  /** Identity of this card's expansion state in the owner's override map. */
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
  const clock = useMemo(() => formatClock(turn.startedAt), [turn.startedAt])
  const stepNoun = `${turn.toolCallCount} step${turn.toolCallCount === 1 ? "" : "s"}`
  const duration = turn.durationMs >= 1000 ? formatDurationMs(turn.durationMs) : null

  const renderSteps = (id: string, steps: Step[]) => (
    <CollapsedStepList key={id} steps={steps} problemsOnly={problemsOnly} now={now} onOpenSubagent={onOpenSubagent} />
  )

  return (
    <section className="overflow-hidden rounded-xl border border-border bg-card">
      <button
        type="button"
        onClick={(e) => {
          anchor(e.currentTarget)
          onToggle(toggleKey, expanded)
        }}
        className="flex w-full items-center gap-2 bg-muted/30 px-3 py-2 text-left hover:bg-muted/50"
      >
        <ChevronRight
          className={cn("size-3.5 shrink-0 text-muted-foreground transition-transform", expanded && "rotate-90")}
        />
        <span className="min-w-0 truncate text-sm font-medium text-foreground">{label}</span>
        <StatusPill status={status} />
        <span className="ml-auto flex shrink-0 items-center gap-2 text-[10.5px] text-muted-foreground">
          {turn.failedCount > 0 && (
            <span className="text-red-400">{turn.failedCount} failed</span>
          )}
          <span>{stepNoun}</span>
          {duration && <span className="font-mono">{duration}</span>}
          <span className="font-mono" suppressHydrationWarning>{clock}</span>
        </span>
      </button>

      {expanded && (
        <div className="space-y-3 px-3 py-3">
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
