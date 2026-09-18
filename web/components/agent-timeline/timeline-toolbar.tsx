"use client"

import { useSyncExternalStore } from "react"
import { cn } from "@/lib/utils"
import type { TimelineStats } from "@/lib/turns"

export type TimelineDensity = "conversation" | "all" | "tools" | "problems"

const DENSITY_STORAGE_KEY = "elasticclaw.timeline.density"

const DENSITY_OPTIONS: { value: TimelineDensity; label: string }[] = [
  { value: "conversation", label: "Conversation" },
  { value: "all", label: "All" },
  { value: "tools", label: "Tools only" },
  { value: "problems", label: "Problems" },
]

function isDensity(value: string | null): value is TimelineDensity {
  return DENSITY_OPTIONS.some((o) => o.value === value)
}

// Tiny external store around localStorage so every open chat shares the same
// persisted choice. The server snapshot is the default; useSyncExternalStore
// reconciles the stored value right after hydration without a mismatch.
let densityCache: TimelineDensity | null = null
const densityListeners = new Set<() => void>()

function readDensity(): TimelineDensity {
  if (densityCache === null) {
    try {
      const stored = localStorage.getItem(DENSITY_STORAGE_KEY)
      densityCache = isDensity(stored) ? stored : "conversation"
    } catch {
      densityCache = "conversation"
    }
  }
  return densityCache
}

function writeDensity(density: TimelineDensity): void {
  densityCache = density
  try {
    localStorage.setItem(DENSITY_STORAGE_KEY, density)
  } catch {}
  densityListeners.forEach((notify) => notify())
}

function subscribeDensity(listener: () => void): () => void {
  densityListeners.add(listener)
  return () => densityListeners.delete(listener)
}

/** Persisted density choice (localStorage-backed, shared across views). */
export function useTimelineDensity(): [TimelineDensity, (d: TimelineDensity) => void] {
  const density = useSyncExternalStore(subscribeDensity, readDensity, () => "conversation" as TimelineDensity)
  return [density, writeDensity]
}

/** Slim toolbar: density segmented control plus plain turns/tools/failures stats. */
export function TimelineToolbar({
  density,
  onDensityChange,
  stats,
}: {
  density: TimelineDensity
  onDensityChange: (d: TimelineDensity) => void
  stats: TimelineStats
}) {
  return (
    <div className="px-3 sm:px-5">
      <div className="mx-auto flex w-full max-w-3xl flex-nowrap items-center justify-between gap-2 py-1.5 sm:gap-3">
        <div className="inline-flex shrink-0 items-center gap-0.5 rounded-control border border-border/60 p-0.5">
          {DENSITY_OPTIONS.map((option) => (
            <button
              key={option.value}
              type="button"
              aria-pressed={density === option.value}
              onClick={() => onDensityChange(option.value)}
              className={cn(
                "h-6 whitespace-nowrap rounded-[calc(var(--control-radius)-2px)] px-1.5 text-[11px] transition-colors max-md:min-h-11 sm:px-2 sm:text-xs",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring",
                density === option.value
                  ? "bg-accent text-foreground"
                  : "text-secondary-label hover:text-foreground"
              )}
            >
              {option.label}
            </button>
          ))}
        </div>
        <span className="flex shrink-0 items-center gap-1 text-[11px] tabular-nums text-muted-foreground sm:gap-1.5 sm:text-xs">
          <span className="max-sm:hidden">{stats.turns} turn{stats.turns === 1 ? "" : "s"}</span>
          <span className="text-border max-sm:hidden">·</span>
          <span>{stats.toolCalls} tool call{stats.toolCalls === 1 ? "" : "s"}</span>
          {stats.failures > 0 && (
            <>
              <span className="text-border">·</span>
              <span className="text-destructive">{stats.failures} failed</span>
            </>
          )}
        </span>
      </div>
    </div>
  )
}
