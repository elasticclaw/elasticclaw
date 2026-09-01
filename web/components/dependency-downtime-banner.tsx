"use client"

import { AlertTriangle } from "lucide-react"
import { DependencyKind, type DependencyStatus } from "@/lib/types"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"
import { formatLLMLimitDeadline } from "@/components/llm-limit-chip"

interface DependencyDowntimeBannerProps {
  dependencies: DependencyStatus[]
  className?: string
}

function bannerText(count: number): string {
  return count === 1 ? "1 dependency with downtime" : `${count} dependencies with downtime`
}

function dependencyKindLabel(kind: DependencyKind): string {
  switch (kind) {
    case DependencyKind.Model:
      return "model"
    case DependencyKind.Sandbox:
      return "sandbox"
    case DependencyKind.IssueTracker:
      return "issue tracker"
    default:
      return kind
  }
}

export function DependencyDowntimeBanner({ dependencies, className }: DependencyDowntimeBannerProps) {
  if (dependencies.length === 0) return null

  const sorted = [...dependencies].sort((a, b) => {
    if (a.kind !== b.kind) return a.kind.localeCompare(b.kind)
    return a.name.localeCompare(b.name)
  })
  const text = bannerText(sorted.length)
  const names = sorted.map((dependency) => dependency.name).join(", ")
  // A dependency that comes back at a known time says so. It is the difference
  // between "go find out what broke" and "wait, it resumes on its own".
  const detail = (dependency: DependencyStatus) =>
    dependency.regainAt ? `until ${formatLLMLimitDeadline(dependency.regainAt)}` : dependencyKindLabel(dependency.kind)
  const title = sorted.map((dependency) => `${dependency.name} - ${detail(dependency)}`).join("\n")

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          aria-label={`${text}: ${names}`}
          title={title}
          className={cn(
            "inline-flex h-7 max-w-full items-center gap-1.5 whitespace-nowrap rounded-md border border-amber-500/40 bg-amber-500/10 px-2.5 text-xs font-medium text-amber-500 outline-none ring-offset-background transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
            className
          )}
        >
          <AlertTriangle className="size-3.5 shrink-0" />
          <span className="truncate">{text}</span>
        </button>
      </TooltipTrigger>
      <TooltipContent side="bottom" align="end" className="max-w-64 text-left">
        <div className="space-y-1">
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
