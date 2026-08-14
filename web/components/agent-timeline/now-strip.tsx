"use client"

import { cn } from "@/lib/utils"
import { formatAge, formatDurationMs, type Step } from "@/lib/turns"
import { useNowTick } from "@/hooks/use-now"
import { useLastOutputAt } from "@/hooks/use-last-output"

/**
 * The agent's "right now" line.
 *
 * `variant="full"` (chat view): sticky strip under the header while the agent
 * is working — what is running, for how long, and how long ago the last output
 * arrived (the staleness signal that tells the user whether the agent is
 * stuck). Subscribes to the per-chunk output store itself: noteOutput fires on
 * every websocket chunk, and only this strip should re-render for that — not
 * the whole chat panel.
 *
 * `variant="card"` (board cards): the same working line at card density, plus
 * non-working states so every card answers "what is this agent doing" at a
 * glance: waiting on the user (its question), finished (what it ended with),
 * error (the failure), offline (last seen). One line, hard truncation.
 */
export type NowStripState = "working" | "waiting" | "done" | "error" | "offline"

const STATE_DOT: Record<Exclude<NowStripState, "working">, string> = {
  waiting: "bg-amber-400",
  done: "bg-muted-foreground/50",
  error: "bg-red-500",
  offline: "bg-muted-foreground/40",
}

const STATE_TEXT: Record<Exclude<NowStripState, "working">, string> = {
  waiting: "text-amber-300/90",
  done: "text-muted-foreground",
  error: "text-red-400",
  offline: "text-muted-foreground/70",
}

export function NowStrip({
  clawId,
  step,
  isStreaming,
  lastMessageAt,
  variant = "full",
  state = "working",
  statusText,
  statusAt,
}: {
  clawId: string
  /** The currently running step (tool or model wait), if any. */
  step: Step | null
  isStreaming: boolean
  /** Timestamp (ms) of the newest durable message — covers the just-opened
   *  case where nothing has streamed yet. */
  lastMessageAt: number
  variant?: "full" | "card"
  /** Card only — "working" mirrors the full strip; other states render `statusText`. */
  state?: NowStripState
  /** Card only: one-line text for waiting/done/error states. */
  statusText?: string
  /** Card only: timestamp (ms) the non-working state refers to, for its age label. */
  statusAt?: number | null
}) {
  const now = useNowTick(true)
  const liveOutputAt = useLastOutputAt(clawId)
  const isCard = variant === "card"

  if (isCard && state !== "working") {
    const dotClass = STATE_DOT[state]
    const textClass = STATE_TEXT[state]
    const text =
      state === "offline"
        ? statusAt
          ? `last seen ${formatAge(statusAt, now)}`
          : "offline"
        : statusText || (state === "error" ? "Agent errored" : "Idle")
    return (
      <div className="flex min-w-0 items-center gap-1.5 border-b border-border bg-muted/20 px-3 py-1 text-[10px] leading-4">
        <span className={cn("size-1.5 shrink-0 rounded-full", dotClass, state === "waiting" && "animate-pulse")} />
        {state === "waiting" && (
          <span className="shrink-0 font-medium text-amber-400">Needs you</span>
        )}
        <span className={cn("min-w-0 flex-1 truncate", textClass)} title={text} suppressHydrationWarning={state === "offline" || undefined}>
          {text}
        </span>
        {state !== "offline" && statusAt != null && statusAt > 0 && (
          <span className="shrink-0 font-mono text-[9.5px] text-muted-foreground" suppressHydrationWarning>
            {formatAge(statusAt, now)}
          </span>
        )}
      </div>
    )
  }

  const lastOutputAt = Math.max(lastMessageAt, liveOutputAt ?? 0) || null
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
    /* Full variant: on phones the detail drops to a second full-width line
       (order-last + basis-full) so the strip wraps to two lines instead of
       truncating away. Card variant: one line, truncate hard. */
    <div
      className={cn(
        "border-b border-border bg-muted/20",
        isCard
          ? "flex min-w-0 items-center gap-1.5 px-3 py-1 text-[10px] leading-4"
          : "flex flex-wrap items-center gap-x-2 gap-y-0.5 px-4 md:px-6 py-1.5 text-xs"
      )}
    >
      <span className={cn("relative flex shrink-0", isCard ? "size-1.5" : "size-2")}>
        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-green-400 opacity-75" />
        <span className={cn("relative inline-flex rounded-full bg-green-500", isCard ? "size-1.5" : "size-2")} />
      </span>
      <span className="shrink-0 font-medium text-foreground">{what}</span>
      {step?.detail && (
        <span
          className={cn(
            "min-w-0 truncate text-muted-foreground",
            !isCard && "max-md:order-last max-md:basis-full max-md:break-all",
            step.detailKind !== "text" && "font-mono"
          )}
          title={step.detail}
        >
          {step.detail}
        </span>
      )}
      <span
        className={cn(
          "ml-auto flex shrink-0 items-center font-mono text-muted-foreground",
          isCard ? "gap-2 text-[9.5px]" : "gap-3 text-[10.5px]"
        )}
      >
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
