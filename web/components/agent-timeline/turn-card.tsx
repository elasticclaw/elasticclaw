"use client"

import { memo, useMemo, type ReactNode } from "react"
import { ChevronRight } from "lucide-react"
import { cn } from "@/lib/utils"
import type { Message } from "@/lib/types"
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
        "inline-flex items-center gap-1 rounded-full border px-1.5 py-px font-mono text-[10px]",
        status === "running" && "border-data/50 text-data",
        status === "failed" && "border-destructive/50 text-destructive",
        status === "ok" && "border-border text-muted-foreground"
      )}
    >
      {status === "running" && (
        <span className="relative flex size-1.5">
          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-data opacity-75" />
          <span className="relative inline-flex size-1.5 rounded-full bg-data" />
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
}: {
  steps: Step[]
  problemsOnly: boolean
  now?: number
}) {
  const items = useMemo(() => {
    const visible = problemsOnly ? steps.filter(isProblemStep) : steps
    return collapseStepRuns(visible)
  }, [steps, problemsOnly])
  if (items.length === 0) return null
  return <StepList items={items} now={now} />
})

/**
 * One turn of the conversation as a card: header with generated label, status
 * pill, step count and duration; body with the prose and the step rows.
 * Collapsed turns keep the prose visible in conversation density — collapsing
 * hides the step rows, not the conversation.
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
  renderMessage,
  now,
  forceRunning,
  children,
}: {
  turn: Turn
  density: TimelineDensity
  /** Whether the step rows are expanded (the turn chevron state). */
  expanded: boolean
  /** Identity of this card's expansion state in the owner's override map. */
  toggleKey: string
  onToggle: (key: string, expanded: boolean) => void
  clawId: string
  renderMessage: (message: Message) => ReactNode
  /** Live clock while this turn has running steps. */
  now?: number
  /** Claw is streaming — the last turn shows as running even between steps. */
  forceRunning?: boolean
  /** Trailing slot (streaming prose) rendered at the end of the body. */
  children?: ReactNode
}) {
  const anchor = useToggleAnchor()
  const showProse = density === "conversation" || density === "all"
  const problemsOnly = density === "problems"
  const status: TurnStatus = turn.status === "ok" && forceRunning ? "running" : turn.status
  const label = useMemo(() => turnLabel(turn), [turn])
  const clock = useMemo(() => formatClock(turn.startedAt), [turn.startedAt])
  const stepNoun = `${turn.toolCallCount} step${turn.toolCallCount === 1 ? "" : "s"}`
  const duration = turn.durationMs >= 1000 ? formatDurationMs(turn.durationMs) : null

  const renderSteps = (id: string, steps: Step[]) => (
    <CollapsedStepList key={id} steps={steps} problemsOnly={problemsOnly} now={now} />
  )

  return (
    /* The mockup's activity group: a hairline-bordered block on a faint ink
       wash, holding the monospaced step rows. */
    <section className="overflow-hidden rounded-lg border border-border bg-foreground/4">
      <button
        type="button"
        onClick={(e) => {
          anchor(e.currentTarget)
          onToggle(toggleKey, expanded)
        }}
        className="flex w-full items-center gap-2 px-3 py-2 text-left transition-colors hover:bg-foreground/7"
      >
        <ChevronRight
          className={cn("size-3.5 shrink-0 text-muted-foreground transition-transform", expanded && "rotate-90")}
        />
        <span className="min-w-0 truncate text-sm font-medium text-foreground">{label}</span>
        <StatusPill status={status} />
        <span className="ml-auto flex shrink-0 items-center gap-2 font-mono text-[10.5px] text-muted-foreground">
          {turn.failedCount > 0 && (
            <span className="text-destructive">{turn.failedCount} failed</span>
          )}
          <span>{stepNoun}</span>
          {duration && <span>{duration}</span>}
          <span suppressHydrationWarning>{clock}</span>
        </span>
      </button>

      {((showProse && (Boolean(turn.userMessage) || turn.items.some((i) => i.type === "message"))) ||
        expanded ||
        Boolean(children)) && (
        <div className="space-y-3 px-3 py-3">
          {showProse && turn.userMessage && renderMessage(turn.userMessage)}
          {turn.items.map((item) => {
            if (item.type === "message") {
              return showProse ? renderMessage(item.message) : null
            }
            if (item.type === "steps") {
              return expanded ? renderSteps(item.id, item.steps) : null
            }
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
          {children}
        </div>
      )}
    </section>
  )
})
