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
import { StepList, type StepDensity } from "./step-row"
import { useToggleAnchor } from "./anchor-context"

function summaryLabel(count: number): string {
  return `${count} earlier tool call${count === 1 ? "" : "s"}`
}

/**
 * Lazy historical tool calls. Timeline `activity_summary` rows fetch their
 * messages via fetchActivityMessages on first expand; runs of live activity
 * rows (board cards) pass `messages` directly. Either way the expansion
 * renders paired step rows.
 */
export function ActivitySummaryBlock({
  clawId,
  messages = [],
  summary,
  density = "full",
}: {
  clawId: string
  /** Already-loaded activity messages (live runs). */
  messages?: Message[]
  /** Lazy summary metadata — present on timeline activity_summary rows. */
  summary?: ActivitySummaryMeta
  density?: StepDensity
}) {
  const [expanded, setExpanded] = useState(false)
  const [loadedMessages, setLoadedMessages] = useState<Message[] | null>(null)
  const [loading, setLoading] = useState(false)
  const anchor = useToggleAnchor()

  const allMessages = useMemo(
    () => [...messages, ...(loadedMessages ?? [])],
    [messages, loadedMessages]
  )
  const stepItems = useMemo(
    () => collapseStepRuns(demoteStaleRunning(pairActivitySteps(allMessages), false)),
    [allMessages]
  )
  const stepCount = useMemo(
    () => stepItems.reduce((sum, item) => sum + (item.type === "group" ? item.steps.length : 1), 0),
    [stepItems]
  )

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

  if (density === "card") {
    return (
      <div className="space-y-1">
        <button
          type="button"
          onClick={(e) => handleToggle(e.currentTarget)}
          className="w-full rounded border border-border/50 bg-muted/20 px-1.5 py-1 text-left text-[10px] text-muted-foreground hover:bg-muted/35"
        >
          {expanded ? "Hide" : "Show"} {label}
        </button>
        {expanded && loading && (
          <div className="px-1.5 text-[10px] text-muted-foreground">Loading tool calls...</div>
        )}
        {expanded && isPartial && (
          <div className="px-1.5 text-[10px] text-muted-foreground">
            Showing latest {loadedCount} of {countOverride} tool calls
          </div>
        )}
        {expanded && <StepList items={stepItems} density="card" />}
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
          className="rounded border border-border/60 bg-muted/25 px-2.5 py-1 text-xs text-muted-foreground hover:bg-muted/40 hover:text-foreground"
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
