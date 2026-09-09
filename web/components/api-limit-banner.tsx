"use client"

import { CircleSlash } from "lucide-react"
import { type DependencyStatus } from "@/lib/types"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"
import { formatLLMLimitDeadline, formatLLMLimitDeadlineUTC } from "@/components/llm-limit-chip"

/**
 * An account out of allowance is not an outage, and it must not borrow the
 * outage badge. The services are up; we cannot spend on them. The operator's
 * next move differs too — an outage is waited out and watched on a status
 * page, a cap is raised in a billing console or waited out to a stated
 * instant — so it gets its own badge and its own count.
 */
interface ApiLimitBannerProps {
  dependencies: DependencyStatus[]
  className?: string
}

function bannerText(count: number): string {
  return count === 1 ? "1 API with no more limits" : `${count} APIs with no more limits`
}

export function ApiLimitBanner({ dependencies, className }: ApiLimitBannerProps) {
  if (dependencies.length === 0) return null

  const sorted = [...dependencies].sort((a, b) => a.name.localeCompare(b.name))
  const text = bannerText(sorted.length)
  const detail = (dependency: DependencyStatus) =>
    dependency.regainAt ? `back ${formatLLMLimitDeadline(dependency.regainAt)}` : "no reset time given"
  const title = sorted
    .map((dependency) =>
      dependency.regainAt
        ? `${dependency.name} - ${detail(dependency)} (${formatLLMLimitDeadlineUTC(dependency.regainAt)})`
        : `${dependency.name} - ${detail(dependency)}`
    )
    .join("\n")

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          aria-label={`${text}: ${sorted.map((dependency) => dependency.name).join(", ")}`}
          title={title}
          className={cn(
            "inline-flex h-7 max-w-full items-center gap-1.5 whitespace-nowrap rounded-md border border-amber-500/40 bg-amber-500/10 px-2.5 text-xs font-medium text-amber-500 outline-none ring-offset-background transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
            className
          )}
        >
          <CircleSlash className="size-3.5 shrink-0" />
          <span className="truncate">{text}</span>
        </button>
      </TooltipTrigger>
      <TooltipContent side="bottom" align="end" className="max-w-72 text-left">
        <div className="space-y-1">
          <p>
            These providers report no allowance left, so agents on them cannot run.
            Raise the account limit to resume sooner.
          </p>
          {sorted.map((dependency) => (
            <div key={dependency.id} className="flex min-w-0 items-center justify-between gap-3">
              <span className="truncate font-medium">{dependency.name}</span>
              <span className="shrink-0 text-background/70">{detail(dependency)}</span>
            </div>
          ))}
        </div>
      </TooltipContent>
    </Tooltip>
  )
}
