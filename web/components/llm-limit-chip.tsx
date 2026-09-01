"use client"

import { CircleSlash } from "lucide-react"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"

/**
 * "This agent is not silent because it is stuck — its model provider is out of
 * allowance, and it comes back at a time we already know."
 *
 * The fleet badge answers "is something wrong somewhere". This answers the
 * question an operator actually arrives with, looking at one quiet agent, and
 * it has to be answerable without opening the chat: during the 2026-08-31
 * block every surface said "connected" while nothing could run.
 */
/**
 * Deadlines render in the reader's own timezone. The question the chip answers
 * is "do I wait, or do I go raise the cap", and nobody answers that in UTC
 * without doing arithmetic first.
 *
 * The zone name is always shown, and the UTC instant stays one hover away (see
 * formatLLMLimitDeadlineUTC): the provider's console and the hub's logs both
 * speak UTC, so an operator reconciling the three still can.
 *
 * These chips only ever render for a claw fetched at runtime, so the static
 * export never prerenders one — no server/client timezone mismatch to guard.
 */
export function formatLLMLimitDeadline(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "an unknown time"
  return new Intl.DateTimeFormat(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZoneName: "short",
  }).format(at)
}

/** The same instant as the provider and the hub logs state it. */
export function formatLLMLimitDeadlineUTC(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "an unknown time"
  return `${at.toISOString().slice(0, 10)} ${at.toISOString().slice(11, 16)} UTC`
}

/**
 * The sidebar row is ~170px wide, where the full deadline truncates to
 * uselessness — a cut-off date is worse than no time at all. Compact drops the
 * year and the zone name, which are the two parts a reader looking at their own
 * clock does not need, and leaves them to the tooltip.
 */
function shortDeadline(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "an unknown time"
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    // The zone survives the shortening. Dropping the year saves room; dropping
    // the zone would leave a bare time that a reader in another timezone — or
    // comparing against the provider console, which speaks UTC — reads wrong.
    timeZoneName: "short",
  }).format(at)
}

interface LLMLimitChipProps {
  /** ISO instant when access returns; omit or pass undefined when not limited. */
  limitedUntil?: string
  className?: string
  /**
   * Compact drops the word "Paused" and shortens the date. Use it wherever a
   * PAUSED state chip already stands next to it — the sidebar row, the
   * conversation header — so the surface does not say "paused" twice. The full
   * form carries the word itself, for surfaces with no state chip at all,
   * where the chip alone has to answer "why is nothing happening".
   */
  compact?: boolean
}

export function LLMLimitChip({ limitedUntil, className, compact = false }: LLMLimitChipProps) {
  if (!limitedUntil) return null
  const deadline = formatLLMLimitDeadline(limitedUntil)
  const label = compact
    ? `API limit · ${shortDeadline(limitedUntil)}`
    : `Paused · no API allowance until ${deadline}`

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          aria-label={`Agent paused: no model provider allowance until ${deadline}`}
          className={cn(
            "inline-flex h-6 max-w-full items-center gap-1.5 whitespace-nowrap rounded-md border border-amber-500/40 bg-amber-500/10 px-2 text-[11px] font-medium text-amber-500",
            className
          )}
        >
          <CircleSlash className="size-3 shrink-0" />
          <span className="truncate">{label}</span>
        </span>
      </TooltipTrigger>
      <TooltipContent side="bottom" className="max-w-72 text-left">
        This agent is stopped: its model provider reports no allowance left, so
        turns are not reaching it. Nothing is broken — the hub resumes it
        automatically at {deadline} ({formatLLMLimitDeadlineUTC(limitedUntil)}),
        and raising the account limit resumes it sooner. Messages you send are
        queued until then.
      </TooltipContent>
    </Tooltip>
  )
}
