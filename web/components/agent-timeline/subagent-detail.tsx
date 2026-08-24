"use client"

import { cn } from "@/lib/utils"
import {
  SUBAGENT_STALE_MS,
  formatAge,
  formatDurationMs,
  type Subagent,
} from "@/lib/subagents"
import {
  SubagentDot,
  SubagentSectionLabel,
  SubagentStatusLabel,
  subagentActivity,
} from "./subagent-status"

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
  if (sub.type) meta.push(sub.type)
  if (sub.model) meta.push(sub.model)
  meta.push(`started ${formatAge(sub.startedAt.getTime(), now)}`)
  meta.push(`${sub.step.messages.length} call${sub.step.messages.length === 1 ? "" : "s"}`)
  if (live) {
    meta.push(`last output ${formatAge(sub.lastOutputAtMs, now)}`)
  } else if (sub.durationMs !== undefined) {
    meta.push(`took ${formatDurationMs(sub.durationMs)}`)
  }

  return (
    <div className={cn("space-y-3", className)}>
      <div className="flex min-w-0 items-center gap-2">
        <SubagentDot status={sub.status} />
        <h2 className="min-w-0 flex-1 truncate font-mono text-sm text-foreground" title={sub.name}>
          {sub.name}
        </h2>
        <SubagentStatusLabel status={sub.status} />
      </div>

      <section className="rounded-lg border border-border bg-card">
        <div className="px-3 pt-2.5">
          <SubagentSectionLabel>Task given by the parent</SubagentSectionLabel>
          <p className="mt-1.5 whitespace-pre-wrap break-words text-[12px] leading-5 text-foreground/90">
            {sub.task || "The parent recorded no task prompt for this subagent."}
          </p>
        </div>
        <div className="mt-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 border-t border-border px-3 py-1.5">
          {meta.map((item, index) => (
            <span key={item} className="flex items-center gap-2 font-mono text-[10px] text-muted-foreground">
              {index > 0 && <span className="text-border">·</span>}
              <span suppressHydrationWarning>{item}</span>
            </span>
          ))}
        </div>
      </section>

      {sub.status === "quiet" && (
        <p
          className="rounded-lg border px-3 py-2 text-[11.5px] leading-5"
          style={{
            borderColor: "color-mix(in srgb, var(--status-idle) 40%, transparent)",
            backgroundColor: "color-mix(in srgb, var(--status-idle) 7%, transparent)",
            color: "var(--text-warning)",
          }}
        >
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
        <div className="flex min-w-0 items-center gap-1.5">
          <activity.Icon className="size-3 shrink-0 text-muted-foreground" />
          <span className="min-w-0 truncate font-mono text-[11px] text-foreground/80" title={activity.text}>
            {activity.text}
          </span>
        </div>
      )}

      {sub.error && (
        <div>
          <SubagentSectionLabel>Error</SubagentSectionLabel>
          <pre className="mt-1 max-h-[50vh] overflow-auto whitespace-pre-wrap break-words rounded-lg border border-red-500/20 bg-red-500/5 p-2.5 font-mono text-[11px] text-red-400">
            {sub.error}
          </pre>
        </div>
      )}

      {sub.result && (
        <div>
          <SubagentSectionLabel>Result</SubagentSectionLabel>
          <pre className="mt-1 max-h-[50vh] overflow-auto whitespace-pre-wrap break-words rounded-lg border border-border/50 bg-muted/40 p-2.5 font-mono text-[11px] text-muted-foreground">
            {sub.result}
          </pre>
        </div>
      )}

      {!sub.result && !sub.error && !live && (
        <p className="text-[11.5px] text-muted-foreground">
          This subagent finished without returning any output.
        </p>
      )}
    </div>
  )
}
