"use client"

import { cn } from "@/lib/utils"
import { formatAge, formatDurationMs, type Subagent } from "@/lib/subagents"
import {
  SubagentChip,
  SubagentDot,
  SubagentStatusLabel,
  subagentActivity,
  subagentCardStyle,
  subagentOutcome,
} from "./subagent-status"

/** Condensed lane row: name + status / activity or task / one duration line. */
function LaneCard({
  sub,
  now,
  onOpen,
}: {
  sub: Subagent
  now: number
  onOpen: (id: string) => void
}) {
  const activity = subagentActivity(sub)
  const live = sub.status === "running" || sub.status === "quiet"
  const line = live
    ? `output ${formatAge(sub.lastOutputAtMs, now)}`
    : sub.durationMs !== undefined
      ? `took ${formatDurationMs(sub.durationMs)}`
      : subagentOutcome(sub)

  return (
    <button
      type="button"
      onClick={() => onOpen(sub.id)}
      style={subagentCardStyle(sub.status)}
      className={cn(
        // min-w-0 is required, not decorative: a flex item defaults to
        // min-width:auto, so an unbreakable mono agent name would widen the
        // card past its 220px basis instead of truncating inside it.
        "flex h-auto w-[220px] min-w-0 shrink-0 grow-0 basis-[220px] flex-col rounded-md",
        "bg-[var(--subagent-wash)] px-1 py-0.5 text-left transition-colors hover:bg-accent/20",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/70"
      )}
    >
      <span className="flex min-h-6 min-w-0 items-center gap-1.5">
        <SubagentDot status={sub.status} />
        <span className="min-w-0 flex-1 truncate text-sm text-foreground/80" title={sub.name}>
          {sub.name}
        </span>
        {sub.model && (
          <SubagentChip title={sub.model} className="max-w-24 text-[.6rem]">
            {sub.model}
          </SubagentChip>
        )}
        <SubagentStatusLabel status={sub.status} className="text-[.6rem]" />
      </span>
      <span className="flex min-w-0 items-center gap-1 text-xs text-muted-foreground">
        {activity ? (
          <>
            <activity.Icon className="size-3 shrink-0 stroke-[1.8]" aria-hidden />
            <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-secondary-label" title={activity.text}>
              {activity.text}
            </span>
          </>
        ) : (
          <span className="min-w-0 flex-1 truncate" title={sub.task}>
            {sub.task || " "}
          </span>
        )}
      </span>
      <span className="truncate font-mono text-[.65rem] tabular-nums text-muted-foreground" suppressHydrationWarning>
        {line}
      </span>
    </button>
  )
}

/**
 * The horizontal lane strip above the transcript.
 *
 * `min-w-0` on the scroller is load-bearing: without it a flex child sized by
 * its content would push the whole column wider and the *page* would scroll
 * sideways instead of the strip.
 */
export function SubagentLanes({
  subagents,
  now,
  onOpen,
  className,
}: {
  subagents: Subagent[]
  /** Live clock (ms) for the age labels. */
  now: number
  onOpen: (id: string) => void
  className?: string
}) {
  if (subagents.length === 0) return null

  return (
    <div
      aria-label="Subagents"
      className={cn(
        "flex w-full min-w-0 gap-1 overflow-x-auto scrollbar-thin",
        "border-b border-border/60 px-3 py-1.5 sm:px-5",
        className
      )}
    >
      {subagents.map((sub) => (
        <LaneCard key={sub.id} sub={sub} now={now} onOpen={onOpen} />
      ))}
    </div>
  )
}
