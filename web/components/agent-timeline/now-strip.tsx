"use client"

import { cn } from "@/lib/utils"
import { formatAge, formatDurationMs, type Step } from "@/lib/turns"
import { useNowTick } from "@/hooks/use-now"

/**
 * Sticky strip under the chat header while the agent is working: what is
 * running right now, for how long, and how long ago the last output arrived —
 * the staleness signal that tells the user whether the agent is stuck.
 */
export function NowStrip({
  step,
  isStreaming,
  lastOutputAt,
}: {
  /** The currently running step (tool or model wait), if any. */
  step: Step | null
  isStreaming: boolean
  /** Timestamp (ms) of the most recent output of any kind. */
  lastOutputAt: number | null
}) {
  const now = useNowTick(true)
  const elapsed = step ? Math.max(0, now - step.startedAt.getTime()) : null
  const outputAge = lastOutputAt !== null ? now - lastOutputAt : null
  // Quiet for a while with something supposedly running — surface it.
  const stale = outputAge !== null && outputAge > 30_000

  const what = step
    ? step.title
    : isStreaming
      ? "Writing a reply"
      : "Working"

  return (
    /* On phones the detail drops to a second full-width line (order-last +
       basis-full) so the strip wraps to two lines instead of truncating away. */
    <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 border-b border-border bg-muted/20 px-4 md:px-6 py-1.5 text-xs">
      <span className="relative flex size-2 shrink-0">
        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-green-400 opacity-75" />
        <span className="relative inline-flex size-2 rounded-full bg-green-500" />
      </span>
      <span className="shrink-0 font-medium text-foreground">{what}</span>
      {step?.detail && (
        <span
          className={cn(
            "min-w-0 truncate text-muted-foreground max-md:order-last max-md:basis-full max-md:break-all",
            step.detailKind !== "text" && "font-mono"
          )}
          title={step.detail}
        >
          {step.detail}
        </span>
      )}
      <span className="ml-auto flex shrink-0 items-center gap-3 font-mono text-[10.5px] text-muted-foreground">
        {elapsed !== null && <span suppressHydrationWarning>{formatDurationMs(elapsed)}</span>}
        {outputAge !== null && (
          <span className={cn(stale && "text-amber-400")} suppressHydrationWarning>
            last output {formatAge(lastOutputAt as number, now)}
          </span>
        )}
      </span>
    </div>
  )
}
