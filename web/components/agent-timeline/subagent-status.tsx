"use client"

import { Bot, FileText, Globe, Search, SquarePen, SquareTerminal, Wrench } from "lucide-react"
import { cn } from "@/lib/utils"
import { activityDetail, activityTitle, toolCategory, type ToolCategory } from "@/lib/turns"
import type { Subagent, SubagentStatus } from "@/lib/subagents"

// The rail, the lane strip and the drill-down must speak one status language —
// three copies of this vocabulary would drift the moment one of them changes.
// Colors are token expressions rather than Tailwind palette classes so the
// left rail, the dot and the label always agree on the same hue.

const STATUS_COLOR: Record<SubagentStatus, string> = {
  running: "var(--step-running)",
  quiet: "var(--status-idle)",
  failed: "var(--status-error)",
  done: "var(--step-done)",
}

/**
 * Text colors for the status word. Only "done" differs from STATUS_COLOR:
 * --step-done is the neutral *border* accent of a finished card (#525252 in
 * dark), ≈2.5:1 as 9.5px text on --card. The status word is the only state
 * indicator a finished card renders, so it uses the meta-text token that was
 * raised precisely to clear 4.5:1 at this size.
 */
const STATUS_TEXT_COLOR: Record<SubagentStatus, string> = {
  ...STATUS_COLOR,
  done: "var(--muted-foreground)",
}

const STATUS_LABEL: Record<SubagentStatus, string> = {
  running: "running",
  quiet: "quiet",
  failed: "failed",
  done: "done",
}

export function subagentColor(status: SubagentStatus): string {
  return STATUS_COLOR[status]
}

/**
 * Left-border + surface wash for a card. Only "quiet" earns a tinted body.
 *
 * The wash is published as `--subagent-wash` rather than as an inline
 * `backgroundColor`: an inline declaration outranks every class, so it also beat
 * the card's `hover:bg-muted/40` and left quiet cards — the ones a user is most
 * likely to click — as the only cards in the rail and lanes with no hover
 * feedback. Cards paint their background from this variable
 * (`bg-[var(--subagent-wash)]`), which the hover utility can override normally.
 */
export function subagentCardStyle(status: SubagentStatus): React.CSSProperties {
  return {
    borderLeftColor: STATUS_COLOR[status],
    "--subagent-wash":
      status === "quiet"
        ? "color-mix(in srgb, var(--status-idle) 7%, transparent)"
        : "color-mix(in srgb, var(--card) 60%, transparent)",
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

/** Uppercase micro status word, colored by status. */
export function SubagentStatusLabel({ status }: { status: SubagentStatus }) {
  return (
    <span
      className="shrink-0 font-mono text-[9.5px] uppercase tracking-[0.06em]"
      style={{ color: STATUS_TEXT_COLOR[status] }}
    >
      {STATUS_LABEL[status]}
    </span>
  )
}

/** Uppercase micro section label — the mock's one recurring typographic anchor. */
export function SubagentSectionLabel({
  children,
  className,
}: {
  children: React.ReactNode
  className?: string
}) {
  return (
    <span
      className={cn(
        "text-[9.5px] uppercase tracking-[0.06em] text-muted-foreground",
        className
      )}
    >
      {children}
    </span>
  )
}

const CATEGORY_ICONS: Record<ToolCategory, typeof Wrench> = {
  read: FileText,
  edit: SquarePen,
  run: SquareTerminal,
  search: Search,
  web: Globe,
  task: Bot,
  other: Wrench,
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
