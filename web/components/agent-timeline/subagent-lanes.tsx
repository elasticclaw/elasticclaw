"use client"

import { cn } from "@/lib/utils"
import { formatAge, formatDurationMs, type Subagent } from "@/lib/subagents"
import {
  SubagentDot,
  SubagentStatusLabel,
  subagentActivity,
  subagentCardStyle,
  subagentOutcome,
} from "./subagent-status"

/** Condensed lane card: name + status / icon + activity / one duration line. */
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
        // card past its 200px basis instead of truncating inside it.
        "flex h-[62px] w-[200px] min-w-0 shrink-0 grow-0 basis-[200px] flex-col justify-center gap-0.5 rounded-[10px] border-l-2",
        "bg-[var(--subagent-wash)] px-2 py-1.5 text-left transition-colors hover:bg-muted/40",
        "focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5">
        <SubagentDot status={sub.status} />
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-foreground" title={sub.name}>
          {sub.name}
        </span>
        <SubagentStatusLabel status={sub.status} />
      </span>
      <span className="flex min-w-0 items-center gap-1">
        {activity ? (
          <>
            <activity.Icon className="size-2.5 shrink-0 text-muted-foreground" />
            <span
              className="min-w-0 flex-1 truncate font-mono text-[10px] text-foreground/80"
              title={activity.text}
            >
              {activity.text}
            </span>
          </>
        ) : (
          <span className="min-w-0 flex-1 truncate text-[10px] text-muted-foreground" title={sub.task}>
            {sub.task || " "}
          </span>
        )}
      </span>
      <span className="truncate font-mono text-[9.5px] text-muted-foreground" suppressHydrationWarning>
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
        "flex w-full min-w-0 gap-2 overflow-x-auto scrollbar-thin",
        "border-b border-border px-3 py-2 md:px-6",
        className
      )}
    >
      {subagents.map((sub) => (
        <LaneCard key={sub.id} sub={sub} now={now} onOpen={onOpen} />
      ))}
    </div>
  )
}
