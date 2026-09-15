"use client"

import { cn } from "@/lib/utils"
import {
  SUBAGENT_STALE_MS,
  formatAge,
  formatDurationMs,
  type Subagent,
} from "@/lib/subagents"
import { EXPANDED_PRE_CLASS } from "./step-row"
import {
  SubagentChip,
  SubagentDot,
  SubagentSectionLabel,
  SubagentStatusLabel,
  subagentActivity,
} from "./subagent-status"

const OUTPUT_BOX_CLASS = "mt-1 rounded-md bg-muted/40 px-3 py-2"

/**
 * The drill-down for one subagent: the task it was given, the facts about the
 * run, and whatever it has produced so far.
 *
 * There is deliberately no kill control here. The mock has one, but the hub
 * has no primitive that can stop a nested session — a button that silently did
 * nothing would be worse than its absence.
 */
export function SubagentDetail({
  subagent,
  now,
  className,
}: {
  subagent: Subagent
  /** Live clock (ms) for the age and staleness labels. */
  now: number
  className?: string
}) {
  const sub = subagent
  const live = sub.status === "running" || sub.status === "quiet"
  const activity = subagentActivity(sub)
  const staleMs = Math.max(0, now - sub.lastOutputAtMs)

  const meta: string[] = []
  meta.push(`started ${formatAge(sub.startedAt.getTime(), now)}`)
  // No call count: `step.messages` holds the parent's own events for this one
  // Task call (start, forwarded progress pulses, terminal), not the calls the
  // subagent made — those never reach the hub. Rendering its length as
  // "N calls" reported "2 calls" for a subagent that ran twenty tools, and moved
  // with gateway pulse frequency rather than with anything the subagent did.
  if (live) {
    meta.push(`last output ${formatAge(sub.lastOutputAtMs, now)}`)
  } else if (sub.durationMs !== undefined) {
    meta.push(`took ${formatDurationMs(sub.durationMs)}`)
  }

  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex min-w-0 items-center gap-2">
        <SubagentDot status={sub.status} />
        <h2 className="min-w-0 flex-1 truncate text-sm text-foreground" title={sub.name}>
          {sub.name}
        </h2>
        {sub.type && <SubagentChip title={sub.type}>{sub.type}</SubagentChip>}
        {sub.model && <SubagentChip title={sub.model}>{sub.model}</SubagentChip>}
        <SubagentStatusLabel status={sub.status} />
      </div>

      <section className="rounded-lg border border-border/70 bg-card/70 p-3">
        <SubagentSectionLabel>Task given by the parent</SubagentSectionLabel>
        <p className="mt-1.5 whitespace-pre-wrap break-words text-sm leading-relaxed text-foreground/80">
          {sub.task || "The parent recorded no task prompt for this subagent."}
        </p>
        <div className="mt-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs tabular-nums text-muted-foreground">
          {meta.map((item, index) => (
            <span key={item} className="flex items-center gap-2">
              {index > 0 && <span className="text-border">·</span>}
              <span suppressHydrationWarning>{item}</span>
            </span>
          ))}
        </div>
      </section>

      {sub.status === "quiet" && (
        <p className="rounded-md bg-warning-surface px-3 py-2 text-xs leading-5 text-warning-foreground">
          <span suppressHydrationWarning>
            No output for {formatDurationMs(staleMs)}
          </span>
          {" — past the "}
          {formatDurationMs(SUBAGENT_STALE_MS)}
          {" threshold. The subagent may be on a long tool call, or it may be stuck. "}
          The parent agent is still waiting on it.
        </p>
      )}

      {activity && (
        <div className="flex min-h-6 min-w-0 items-center gap-1.5 px-0.5">
          <span className="flex size-6 shrink-0 items-center justify-center text-icon-muted">
            <activity.Icon className="size-4 shrink-0 stroke-[1.8]" aria-hidden />
          </span>
          <span className="min-w-0 truncate font-mono text-[13px] live-tool-shine" title={activity.text}>
            {activity.text}
          </span>
        </div>
      )}

      {sub.error && (
        <div>
          <SubagentSectionLabel>Error</SubagentSectionLabel>
          <div className={OUTPUT_BOX_CLASS}>
            <pre className={cn(EXPANDED_PRE_CLASS, "max-h-[50vh] text-[11px] text-error-foreground/80")}>{sub.error}</pre>
          </div>
        </div>
      )}

      {sub.result && (
        <div>
          <SubagentSectionLabel>Result</SubagentSectionLabel>
          <div className={OUTPUT_BOX_CLASS}>
            <pre className={cn(EXPANDED_PRE_CLASS, "max-h-[50vh] text-[11px] text-secondary-label")}>{sub.result}</pre>
          </div>
        </div>
      )}

      {!sub.result && !sub.error && !live && (
        <p className="text-xs text-muted-foreground">
          This subagent finished without returning any output.
        </p>
      )}
    </div>
  )
}
