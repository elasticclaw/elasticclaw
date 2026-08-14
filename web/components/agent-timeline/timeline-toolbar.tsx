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

/** Slim toolbar: density segmented control plus a turns/tools/failures chip. */
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
    <div className="flex items-center justify-between gap-3 border-b border-border px-6 py-1.5">
      <div className="flex items-center gap-0.5 rounded-md border border-border bg-muted/30 p-0.5">
        {DENSITY_OPTIONS.map((option) => (
          <button
            key={option.value}
            type="button"
            onClick={() => onDensityChange(option.value)}
            className={cn(
              "rounded px-2 py-0.5 text-xs transition-colors",
              density === option.value
                ? "bg-secondary font-medium text-foreground"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            {option.label}
          </button>
        ))}
      </div>
      <span className="flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-muted/20 px-2.5 py-0.5 text-[10.5px] text-muted-foreground">
        <span>{stats.turns} turn{stats.turns === 1 ? "" : "s"}</span>
        <span className="text-border">·</span>
        <span>{stats.toolCalls} tool call{stats.toolCalls === 1 ? "" : "s"}</span>
        {stats.failures > 0 && (
          <>
            <span className="text-border">·</span>
            <span className="font-medium text-red-400">{stats.failures} failed</span>
          </>
        )}
      </span>
    </div>
  )
}
