"use client"

import { cn } from "@/lib/utils"
import { formatAge, formatDurationMs, type Step } from "@/lib/turns"
import { useNowTick } from "@/hooks/use-now"
import { useLastOutputAt } from "@/hooks/use-last-output"

/**
 * The agent's "right now" line.
 *
 * `variant="full"` (chat view): the "Working for …" row under the header while
 * the agent is working — what is running, for how long, and how long ago the
 * last output arrived (the staleness signal that tells the user whether the
 * agent is stuck). Subscribes to the per-chunk output store itself: noteOutput
 * fires on every websocket chunk, and only this row should re-render for that
 * — not the whole chat panel.
 *
 * `variant="card"` (board cards): the same working line at card density, plus
 * non-working states so every card answers "what is this agent doing" at a
 * glance: waiting on the user (its question), finished (what it ended with),
 * error (the failure), offline (last seen). One line, hard truncation.
 */
export type NowStripState = "working" | "waiting" | "done" | "error" | "offline"

const STATE_DOT: Record<Exclude<NowStripState, "working">, string> = {
  waiting: "bg-[var(--status-idle)]",
  done: "bg-muted-foreground/50",
  error: "bg-[var(--status-error)]",
  offline: "bg-muted-foreground/50",
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
    const text =
      state === "offline"
        ? statusAt
          ? `last seen ${formatAge(statusAt, now)}`
          : "offline"
        : statusText || (state === "error" ? "Agent errored" : "Idle")
    return (
      <div className="flex min-w-0 items-center gap-1.5 border-b border-border/60 px-3 py-1 text-xs leading-4 text-muted-foreground">
        <span className={cn("size-1.5 shrink-0 rounded-full", STATE_DOT[state], state === "waiting" && "animate-pulse")} />
        {state === "waiting" && (
          <span className="shrink-0 font-medium text-warning-foreground">Needs you</span>
        )}
        <span className="min-w-0 flex-1 truncate" title={text} suppressHydrationWarning={state === "offline" || undefined}>
          {text}
        </span>
        {state !== "offline" && statusAt != null && statusAt > 0 && (
          <span className="shrink-0 font-mono text-[.7rem] tabular-nums" suppressHydrationWarning>
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

  /* Full variant: on phones the detail drops to a second full-width line
     (order-last + basis-full) and the output age joins the first line after a
     separator. Card variant: one line, truncate hard. */
  const row = (
    <div
      className={cn(
        "flex min-w-0 items-baseline gap-x-2 tabular-nums text-muted-foreground",
        isCard
          ? "px-3 py-1 text-xs leading-4"
          : "min-h-6 flex-wrap gap-y-0 px-1 text-sm leading-relaxed"
      )}
    >
      <span className="shrink-0 whitespace-nowrap">
        {elapsed !== null ? (
          <>
            Working for <span suppressHydrationWarning>{formatDurationMs(elapsed)}</span>
          </>
        ) : (
          "Working"
        )}
      </span>
      <span
        className={cn(
          "min-w-0 flex-1 truncate live-tool-shine",
          !isCard && "max-md:order-last max-md:basis-full"
        )}
        title={step?.detail || what}
      >
        {what}
        {step?.detail && (
          <span className={cn("ml-1.5", step.detailKind !== "text" && "font-mono text-[13px]")}>{step.detail}</span>
        )}
      </span>
      {outputAge !== null && (
        <>
          {!isCard && <span className="-mx-1 text-border md:hidden" aria-hidden>·</span>}
          <span
            className={cn(
              "shrink-0 font-mono text-[.7rem] tabular-nums md:ml-auto",
              stale ? "text-warning-foreground" : "text-muted-foreground"
            )}
            suppressHydrationWarning
          >
            last output {formatAge(lastOutputAt as number, now)}
          </span>
        </>
      )}
    </div>
  )

  if (isCard) return <div className="border-b border-border/60">{row}</div>
  return (
    <div className="border-b border-border/60 px-3 sm:px-5">
      <div className="mx-auto w-full max-w-3xl pb-2 pt-1">{row}</div>
    </div>
  )
}
