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
export function formatLLMLimitDeadline(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "an unknown time"
  // UTC, to the minute, matching how the provider states it. A local rendering
  // would silently disagree with the provider's own console and with the hub
  // logs, which is the last thing an operator comparing the two needs.
  const date = at.toISOString().slice(0, 10)
  const clock = at.toISOString().slice(11, 16)
  return `${date} ${clock} UTC`
}

/**
 * The sidebar row is ~170px wide, where the full deadline truncates to
 * uselessness — a cut-off "back 2026-09-01 …" is worse than no time at all.
 * Compact keeps the day and the clock, which is what an operator reads to
 * decide whether to wait, and leaves the year and the reasoning to the tooltip.
 */
function shortDeadline(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "an unknown time"
  const month = at.toLocaleString("en-US", { month: "short", timeZone: "UTC" })
  return `${month} ${at.getUTCDate()}, ${at.toISOString().slice(11, 16)} UTC`
}

interface LLMLimitChipProps {
  /** ISO instant when access returns; omit or pass undefined when not limited. */
  limitedUntil?: string
  className?: string
  /** Compact trades the full date for one that fits a dense row. */
  compact?: boolean
}

export function LLMLimitChip({ limitedUntil, className, compact = false }: LLMLimitChipProps) {
  if (!limitedUntil) return null
  const deadline = formatLLMLimitDeadline(limitedUntil)
  const label = compact ? `API limit · ${shortDeadline(limitedUntil)}` : `API limit · back ${deadline}`

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          aria-label={`Model provider out of allowance until ${deadline}`}
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
        The model provider reports this account is out of allowance, so turns are
        not reaching the agent. Nothing is broken — the hub resumes automatically
        at {deadline}. Messages you send are queued until then.
      </TooltipContent>
    </Tooltip>
  )
}
