"use client"

import { cn } from "@/lib/utils"
import {
  formatAge,
  formatDurationMs,
  subagentCounts,
  type Subagent,
} from "@/lib/subagents"
import {
  SubagentDot,
  SubagentSectionLabel,
  SubagentStatusLabel,
  subagentActivity,
  subagentCardStyle,
  subagentOutcome,
} from "./subagent-status"

/**
 * One subagent card in the rail.
 *
 * A real <button>, not a div with a click handler: the rail is a list of
 * destinations, and Tab/Enter must reach every one of them. Every text field
 * truncates — a 90-character agent name or a pasted path must never widen the
 * rail or wrap it into a five-line block.
 */
function RailCard({
  sub,
  now,
  onOpen,
}: {
  sub: Subagent
  now: number
  onOpen: (id: string) => void
}) {
  const activity = subagentActivity(sub)
  const parts = [sub.model, formatAge(sub.startedAt.getTime(), now)].filter(Boolean) as string[]
  if (sub.status === "running" || sub.status === "quiet") {
    parts.push(`output ${formatAge(sub.lastOutputAtMs, now)}`)
  } else if (sub.durationMs !== undefined) {
    parts.push(`took ${formatDurationMs(sub.durationMs)}`)
  }

  return (
    <button
      type="button"
      onClick={() => onOpen(sub.id)}
      style={subagentCardStyle(sub.status)}
      className={cn(
        "block w-full min-w-0 rounded-md border-l-2 bg-card/60 px-2 py-1.5 text-left",
        "transition-colors hover:bg-muted/40",
        "focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5">
        <SubagentDot status={sub.status} />
        <span className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-foreground" title={sub.name}>
          {sub.name}
        </span>
        <SubagentStatusLabel status={sub.status} />
      </span>
      {sub.task && (
        <span className="mt-1 line-clamp-2 text-[11px] leading-4 text-muted-foreground" title={sub.task}>
          {sub.task}
        </span>
      )}
      {activity && (
        <span className="mt-1 flex min-w-0 items-center gap-1">
          <activity.Icon className="size-2.5 shrink-0 text-muted-foreground" />
          <span className="min-w-0 flex-1 truncate font-mono text-[10px] text-foreground/80" title={activity.text}>
            {activity.text}
          </span>
        </span>
      )}
      {parts.length > 0 && (
        <span className="mt-1 block truncate font-mono text-[9.5px] text-muted-foreground" suppressHydrationWarning>
          {parts.join(" · ")}
        </span>
      )}
    </button>
  )
}

/** Finished subagents collapse to name + status + one line of outcome. */
function FinishedCard({ sub, onOpen }: { sub: Subagent; onOpen: (id: string) => void }) {
  const outcome = subagentOutcome(sub)
  return (
    <button
      type="button"
      onClick={() => onOpen(sub.id)}
      style={subagentCardStyle(sub.status)}
      className={cn(
        "block w-full min-w-0 rounded-md border-l-2 px-2 py-1 text-left",
        "transition-colors hover:bg-muted/40",
        "focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
      )}
    >
      <span className="flex min-w-0 items-center gap-1.5">
        <span
          className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted-foreground"
          title={sub.name}
        >
          {sub.name}
        </span>
        <SubagentStatusLabel status={sub.status} />
      </span>
      <span
        className={cn(
          "mt-0.5 block truncate font-mono text-[10px]",
          sub.status === "failed" ? "text-[var(--text-error)]" : "text-muted-foreground/80"
        )}
        title={outcome}
      >
        {outcome}
      </span>
    </button>
  )
}

/**
 * The right-hand subagent rail: live subagents on top, finished ones below a
 * divider label. It owns its own scroll so 50 subagents stay usable without
 * stretching the transcript column, and its width is capped in vw so it never
 * eats the reading measure on a narrow desktop window.
 */
export function SubagentRail({
  subagents,
  now,
  onOpen,
  className,
}: {
  subagents: Subagent[]
  /** Live clock (ms) for the age/staleness labels. */
  now: number
  onOpen: (id: string) => void
  className?: string
}) {
  const counts = subagentCounts(subagents)
  // collectSubagents already sorts running → quiet → finished, but the split
  // is by status, not by position: never assume the sort to slice the list.
  const active = subagents.filter((s) => s.status === "running" || s.status === "quiet")
  const finished = subagents.filter((s) => s.status === "done" || s.status === "failed")

  return (
    <aside
      aria-label="Subagents"
      className={cn(
        "flex w-[min(300px,22vw)] min-w-[196px] shrink-0 flex-col overflow-y-auto",
        "scrollbar-thin border-l border-border",
        className
      )}
    >
      <div className="sticky top-0 z-10 flex items-baseline gap-2 border-b border-border bg-background px-2.5 py-1.5">
        <SubagentSectionLabel>Subagents</SubagentSectionLabel>
        <span className="ml-auto truncate font-mono text-[9.5px] text-muted-foreground">
          {counts.running + counts.quiet} running · {counts.done + counts.failed} finished
        </span>
      </div>

      <div className="flex flex-col gap-1.5 p-2">
        {subagents.length === 0 && (
          <p className="px-1 py-4 text-[11px] leading-4 text-muted-foreground">
            No subagents in this transcript yet.
          </p>
        )}
        {active.map((sub) => (
          <RailCard key={sub.id} sub={sub} now={now} onOpen={onOpen} />
        ))}
        {finished.length > 0 && (
          <>
            {active.length > 0 && <div className="mt-1 h-px bg-border" />}
            <SubagentSectionLabel className="px-1 pt-0.5">Finished</SubagentSectionLabel>
            {finished.map((sub) => (
              <FinishedCard key={sub.id} sub={sub} onOpen={onOpen} />
            ))}
          </>
        )}
      </div>
    </aside>
  )
}
