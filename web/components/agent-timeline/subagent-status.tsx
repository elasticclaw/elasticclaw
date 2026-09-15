"use client"

import { Wrench } from "lucide-react"
import { cn } from "@/lib/utils"
import { activityDetail, activityTitle, toolCategory } from "@/lib/turns"
import type { Subagent, SubagentStatus } from "@/lib/subagents"
import { CATEGORY_ICONS } from "./step-row"

// The rail, the lane strip and the drill-down must speak one status language —
// three copies of this vocabulary would drift the moment one of them changes.
// Dot colors are token expressions rather than Tailwind palette classes so the
// dot always agrees with the rest of the hub's status hues.

const STATUS_COLOR: Record<SubagentStatus, string> = {
  running: "var(--step-running)",
  quiet: "var(--status-idle)",
  failed: "var(--status-error)",
  done: "var(--step-done)",
}

const STATUS_LABEL: Record<SubagentStatus, string> = {
  running: "Working",
  quiet: "Quiet",
  failed: "Failed",
  done: "Completed",
}

/** The status word only colors the two states that need attention. */
const STATUS_TEXT_CLASS: Record<SubagentStatus, string> = {
  running: "text-muted-foreground",
  quiet: "text-warning-foreground",
  failed: "text-destructive",
  done: "text-muted-foreground",
}

export function subagentColor(status: SubagentStatus): string {
  return STATUS_COLOR[status]
}

/**
 * Surface wash for a card. Only a running subagent earns a tint, and a faint
 * one — the rail is a list, not a dashboard.
 *
 * The wash is published as `--subagent-wash` rather than as an inline
 * `backgroundColor`: an inline declaration outranks every class, so it would
 * also beat the card's hover utility and leave running cards — the ones a
 * user is most likely to click — as the only rows with no hover feedback.
 * Cards paint their background from this variable
 * (`bg-[var(--subagent-wash)]`), which the hover utility can override normally.
 */
export function subagentCardStyle(status: SubagentStatus): React.CSSProperties {
  return {
    "--subagent-wash":
      status === "running"
        ? "color-mix(in srgb, var(--step-running) 6%, transparent)"
        : "transparent",
  } as React.CSSProperties
}

/** 5px status dot; pulses while the subagent is running or has gone quiet. */
export function SubagentDot({ status, className }: { status: SubagentStatus; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn(
        "size-[5px] shrink-0 rounded-full",
        (status === "quiet" || status === "running") && "animate-subagent-pulse",
        className
      )}
      style={{ backgroundColor: STATUS_COLOR[status] }}
    />
  )
}

/** Sentence-case mono status word. */
export function SubagentStatusLabel({ status, className }: { status: SubagentStatus; className?: string }) {
  return (
    <span className={cn("shrink-0 font-mono text-[.7rem] tabular-nums", STATUS_TEXT_CLASS[status], className)}>
      {STATUS_LABEL[status]}
    </span>
  )
}

/** Quiet section label. */
export function SubagentSectionLabel({
  children,
  className,
}: {
  children: React.ReactNode
  className?: string
}) {
  return <span className={cn("text-xs text-muted-foreground", className)}>{children}</span>
}

/** Role/model chip shared by the rail, the lanes and the drill-down. */
export function SubagentChip({
  children,
  title,
  className,
}: {
  children: React.ReactNode
  title?: string
  className?: string
}) {
  return (
    <span
      className={cn(
        "max-w-28 shrink-0 truncate rounded-sm border border-border/60 px-1 font-mono text-[.65rem] text-muted-foreground",
        className
      )}
      title={title}
    >
      {children}
    </span>
  )
}

export interface SubagentActivity {
  Icon: typeof Wrench
  text: string
}

/**
 * The "what is it doing right now" line.
 *
 * The honest source is the newest event on the Task step itself — the nested
 * session's own tool calls do not reach the hub as separate rows, so this is
 * the detail the bridge last forwarded, not a fabricated inner trace. Finished
 * subagents get nothing: their outcome, not their last breath, is the story.
 */
export function subagentActivity(sub: Subagent): SubagentActivity | null {
  if (sub.status !== "running" && sub.status !== "quiet") return null
  const last = sub.step.messages[sub.step.messages.length - 1]
  if (!last) return null
  const text = activityDetail(last) || activityTitle(last)
  if (!text.trim()) return null
  return { Icon: CATEGORY_ICONS[toolCategory(last)] ?? Wrench, text }
}

/** One-line outcome for a finished subagent (result head, or the error). */
export function subagentOutcome(sub: Subagent): string {
  const raw = sub.error || sub.result || ""
  const firstLine = raw.split("\n").find((line) => line.trim()) || ""
  return firstLine.trim() || (sub.status === "failed" ? "failed with no output" : "no output")
}
