"use client"

import { useMemo, useState } from "react"
import { ChevronRight, History } from "lucide-react"
import { cn } from "@/lib/utils"
import { fetchActivityMessages } from "@/lib/api"
import { mapApiMessage } from "@/lib/mappers"
import type { ActivitySummary as ActivitySummaryMeta, Message } from "@/lib/types"
import {
  collapseStepRuns,
  demoteStaleRunning,
  pairActivitySteps,
} from "@/lib/turns"
import { ROW_INTERACTIVE_CLASS, StepList, StepRow, type StepDensity } from "./step-row"
import { useToggleAnchor } from "./anchor-context"

function summaryLabel(count: number): string {
  return `${count} earlier tool call${count === 1 ? "" : "s"}`
}

/** Board cards always show the last few steps as real rows; the rest collapse. */
const CARD_TRAILING_STEPS = 4

/** The "N earlier tool calls" affordance, in the same 24px row anatomy as a step. */
function SummaryRow({
  label,
  expanded,
  density,
  onToggle,
}: {
  label: string
  expanded: boolean
  density: StepDensity
  onToggle: (el: HTMLElement) => void
}) {
  const isCard = density === "card"
  return (
    <button
      type="button"
      aria-expanded={expanded}
      onClick={(e) => onToggle(e.currentTarget)}
      className={cn(
        "flex w-full items-center gap-1.5 rounded-md px-0.5 py-0.5 text-left leading-relaxed transition-colors",
        isCard ? "min-h-5 text-xs" : "min-h-6 text-sm",
        "max-md:min-h-11",
        ROW_INTERACTIVE_CLASS
      )}
    >
      <span className={cn("flex shrink-0 items-center justify-center text-icon-muted", isCard ? "size-5" : "size-6")}>
        <History className={cn("shrink-0 stroke-[1.8]", isCard ? "size-3.5" : "size-4")} aria-hidden />
      </span>
      <span className="min-w-0 flex-1 truncate text-secondary-label">
        {expanded ? "Hide" : "Show"} {label}
      </span>
      <span className="flex size-4 shrink-0 items-center justify-center" aria-hidden>
        <ChevronRight
          className={cn("size-3 shrink-0 text-icon-muted opacity-70 transition-transform duration-200", expanded && "rotate-90")}
        />
      </span>
    </button>
  )
}

/**
 * Historical tool calls. At full density: a lazy row — timeline
 * `activity_summary` rows fetch their messages via fetchActivityMessages on
 * first expand; runs of live activity rows pass `messages` directly.
 *
 * At card density the trailing steps render as always-visible compact rows
 * (tool, target, duration, exit code) so a board card shows what the agent is
 * doing without any clicks; only the older calls sit behind one "N earlier"
 * affordance.
 */
export function ActivitySummaryBlock({
  clawId,
  messages = [],
  summary,
  density = "full",
  now,
  keepTrailingRunning = false,
}: {
  clawId: string
  /** Already-loaded activity messages (live runs). */
  messages?: Message[]
  /** Lazy summary metadata — present on timeline activity_summary rows. */
  summary?: ActivitySummaryMeta
  density?: StepDensity
  /** Live clock (ms) so a running trailing step keeps ticking (card density). */
  now?: number
  /** True only for the transcript's trailing run of a live claw. */
  keepTrailingRunning?: boolean
}) {
  const [expanded, setExpanded] = useState(false)
  const [loadedMessages, setLoadedMessages] = useState<Message[] | null>(null)
  const [loading, setLoading] = useState(false)
  const anchor = useToggleAnchor()

  const allMessages = useMemo(
    () => [...messages, ...(loadedMessages ?? [])],
    [messages, loadedMessages]
  )
  const steps = useMemo(
    () => demoteStaleRunning(pairActivitySteps(allMessages), keepTrailingRunning),
    [allMessages, keepTrailingRunning]
  )
  const stepItems = useMemo(() => collapseStepRuns(steps), [steps])
  const stepCount = steps.length

  const countOverride = summary?.count
  const loadedCount = loadedMessages?.length ?? messages.length
  const isPartial = Boolean(countOverride && loadedMessages && loadedCount < countOverride)
  const label = summaryLabel(countOverride && countOverride > 0 ? countOverride : stepCount)

  const handleToggle = (el: HTMLElement) => {
    anchor(el)
    setExpanded((v) => !v)
    if (expanded || !summary || loadedMessages || loading) return
    const summaryCount = summary.count || 0
    const limit = Math.max(200, Math.min(summaryCount || 200, 500))
    const newestFirst = summaryCount > limit
    setLoading(true)
    fetchActivityMessages(clawId, {
      from: summary.from,
      to: summary.to,
      limit,
      order: newestFirst ? "desc" : "asc",
    })
      .then((apiMsgs) => {
        const mapped = apiMsgs.map(mapApiMessage)
        setLoadedMessages(newestFirst ? mapped.reverse() : mapped)
      })
      .catch(console.warn)
      .finally(() => setLoading(false))
  }

  // Fully-expanded remainder placeholders (count 0, no rows) have nothing to
  // show or fetch — they only exist so timeline re-merges stay deduplicated.
  if (stepCount === 0 && !(countOverride && countOverride > 0)) return null

  const noteClass = cn("ms-7 py-0.5 text-muted-foreground", density === "card" ? "text-[10px]" : "text-xs")

  if (density === "card") {
    const trailing = expanded ? [] : steps.slice(-CARD_TRAILING_STEPS)
    // Older calls: loaded steps above the trailing window, plus whatever is
    // still behind the lazy summary fetch.
    const remoteCount = countOverride
      ? Math.max(0, countOverride - (loadedMessages?.length ?? 0))
      : 0
    const earlierCount = Math.max(0, stepCount - CARD_TRAILING_STEPS) + remoteCount
    const showEarlier = expanded || earlierCount > 0
    return (
      <div className="flex flex-col">
        {showEarlier && (
          <SummaryRow label={summaryLabel(earlierCount)} expanded={expanded} density="card" onToggle={handleToggle} />
        )}
        {expanded && loading && <div className={noteClass}>Loading tool calls...</div>}
        {expanded && isPartial && (
          <div className={noteClass}>
            Showing latest {loadedCount} of {countOverride} tool calls
          </div>
        )}
        {expanded ? (
          <StepList items={stepItems} density="card" now={now} />
        ) : (
          trailing.map((step) => (
            <StepRow key={step.id} step={step} density="card" now={now} />
          ))
        )}
      </div>
    )
  }

  return (
    <div className="flex flex-col pb-1">
      <SummaryRow label={label} expanded={expanded} density="full" onToggle={handleToggle} />
      {expanded && loading && <div className={noteClass}>Loading tool calls...</div>}
      {expanded && isPartial && (
        <div className={noteClass}>
          Showing latest {loadedCount} of {countOverride} tool calls
        </div>
      )}
      {expanded && <StepList items={stepItems} />}
    </div>
  )
}
