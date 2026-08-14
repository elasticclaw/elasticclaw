"use client"

import type { ReactNode } from "react"
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

/**
 * One turn of the conversation as a card: header with generated label, status
 * pill, step count and duration; body with the prose and the step rows.
 * Collapsed turns keep the prose visible in conversation density — collapsing
 * hides the step rows, not the conversation.
 */
export function TurnCard({
  turn,
  density,
  expanded,
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
  onToggle: () => void
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
  const label = turnLabel(turn)
  const stepNoun = `${turn.toolCallCount} step${turn.toolCallCount === 1 ? "" : "s"}`
  const duration = turn.durationMs >= 1000 ? formatDurationMs(turn.durationMs) : null

  const renderSteps = (id: string, steps: Step[]) => {
    const visible = problemsOnly ? steps.filter(isProblemStep) : steps
    if (visible.length === 0) return null
    return <StepList key={id} items={collapseStepRuns(visible)} now={now} />
  }

  return (
    <section className="overflow-hidden rounded-xl border border-border bg-card">
      <button
        type="button"
        onClick={(e) => {
          anchor(e.currentTarget)
          onToggle()
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
          <span className="font-mono" suppressHydrationWarning>{formatClock(turn.startedAt)}</span>
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
}
