"use client"

import { useMemo, useState } from "react"
import { fetchActivityMessages } from "@/lib/api"
import { mapApiMessage } from "@/lib/mappers"
import type { ActivitySummary as ActivitySummaryMeta, Message } from "@/lib/types"
import {
  collapseStepRuns,
  demoteStaleRunning,
  pairActivitySteps,
} from "@/lib/turns"
import { StepList, StepRow, type StepDensity } from "./step-row"
import { useToggleAnchor } from "./anchor-context"

function summaryLabel(count: number): string {
  return `${count} earlier tool call${count === 1 ? "" : "s"}`
}

/** Board cards always show the last few steps as real rows; the rest collapse. */
const CARD_TRAILING_STEPS = 4

/**
 * Historical tool calls. At full density: a lazy divider — timeline
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
      <div className="space-y-0.5">
        {showEarlier && (
          <button
            type="button"
            onClick={(e) => handleToggle(e.currentTarget)}
            className="w-full border border-border px-1.5 py-0.5 text-left font-mono text-[10px] text-muted-foreground hover:bg-foreground/5"
          >
            {expanded ? "Hide" : "Show"} {summaryLabel(earlierCount)}
          </button>
        )}
        {expanded && loading && (
          <div className="px-1.5 text-[10px] text-muted-foreground">Loading tool calls...</div>
        )}
        {expanded && isPartial && (
          <div className="px-1.5 text-[10px] text-muted-foreground">
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
    <div className="space-y-1">
      <div className="flex items-center gap-2 py-1">
        <div className="h-px flex-1 bg-border/50" />
        <button
          type="button"
          onClick={(e) => handleToggle(e.currentTarget)}
          className="border border-border px-2.5 py-1 font-mono text-xs text-muted-foreground hover:bg-foreground/5 hover:text-foreground"
        >
          {expanded ? "Hide" : "Show"} {label}
        </button>
        <div className="h-px flex-1 bg-border/50" />
      </div>
      {expanded && loading && (
        <div className="text-center text-xs text-muted-foreground">Loading tool calls...</div>
      )}
      {expanded && isPartial && (
        <div className="text-center text-xs text-muted-foreground">
          Showing latest {loadedCount} of {countOverride} tool calls
        </div>
      )}
      {expanded && <StepList items={stepItems} />}
    </div>
  )
}
