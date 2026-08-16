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
  waiting: "bg-status-warn",
  done: "bg-muted-foreground",
  error: "bg-destructive",
  offline: "bg-muted-foreground/60",
}

const STATE_TEXT: Record<Exclude<NowStripState, "working">, string> = {
  waiting: "text-status-warn",
  done: "text-muted-foreground",
  error: "text-destructive",
  offline: "text-muted-foreground",
}

/** Geometry of the board card's state row — no type utilities here, so the
 *  quiet (mono) and loud (Archivo 800) variants never fight over the cascade:
 *  same-property Tailwind utilities win by source order in the sheet, not by
 *  the order they appear in a className string. */
const STATE_ROW =
  "flex min-w-0 items-center gap-1.5 border-b border-border px-3 py-1 leading-4"
const STATE_ROW_QUIET = "font-mono text-[10px]"
const STATE_ROW_LOUD = "font-sans text-[11px] font-extrabold uppercase tracking-[0.1em]"

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
    // The two loud states get the mockup's full-width state strip: a solid
    // accent band for "needs you", the deep accent tint for a failure. The
    // quiet states stay a plain mono line so the board reads at a glance.
    const isBanner = state === "waiting" || state === "error"
    return (
      <div
        className={cn(
          STATE_ROW,
          isBanner ? STATE_ROW_LOUD : STATE_ROW_QUIET,
          state === "waiting" && "bg-primary text-primary-foreground",
          state === "error" && "bg-[var(--ds-accent-800)] text-[var(--ds-accent-100)]"
        )}
      >
        {!isBanner && <span className={cn("size-1.5 shrink-0 rounded-full", dotClass)} />}
        {state === "waiting" && <span className="shrink-0">Waiting on you</span>}
        <span
          className={cn(
            "min-w-0 flex-1 truncate",
            state === "waiting" && "font-mono text-[10px] font-normal normal-case tracking-normal opacity-85",
            !isBanner && textClass
          )}
          title={text}
          suppressHydrationWarning={state === "offline" || undefined}
        >
          {text}
        </span>
        {state !== "offline" && statusAt != null && statusAt > 0 && (
          <span
            className={cn(
              "shrink-0 font-mono text-[9.5px] font-normal normal-case tracking-normal",
              isBanner ? "opacity-85" : "text-muted-foreground"
            )}
            suppressHydrationWarning
          >
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
        "border-b border-border",
        isCard
          ? cn(STATE_ROW, STATE_ROW_QUIET, "text-status-ok")
          : "flex flex-wrap items-center gap-x-2 gap-y-0.5 bg-foreground/4 px-4 md:px-6 py-1.5 text-xs"
      )}
    >
      <span className={cn("relative flex shrink-0", isCard ? "size-1.5" : "size-2")}>
        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-status-ok opacity-75" />
        <span className={cn("relative inline-flex rounded-full bg-status-ok", isCard ? "size-1.5" : "size-2")} />
      </span>
      <span className={cn("shrink-0", isCard ? "text-status-ok" : "font-medium text-foreground")}>{what}</span>
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
          <span className={cn(stale && "text-status-warn")} suppressHydrationWarning>
            last output {formatAge(lastOutputAt as number, now)}
          </span>
        )}
      </span>
    </div>
  )
}
