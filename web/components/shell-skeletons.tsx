"use client"

import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"

// Placeholders for the agents shell while its data is still unknown. Each one
// takes the footprint of what replaces it, so nothing moves when data lands.

export function SidebarListSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div className="space-y-1 px-3 py-2" aria-label="Loading agents">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="flex items-center gap-2 py-2">
          <Skeleton className="size-2 rounded-full" />
          <Skeleton className="h-4 flex-1" />
        </div>
      ))}
    </div>
  )
}

export function BoardSkeleton({ isMobile = false }: { isMobile?: boolean }) {
  return (
    <div
      className={cn(
        "h-full p-3",
        isMobile ? "space-y-3 overflow-hidden" : "flex gap-6 overflow-hidden items-stretch"
      )}
      aria-label="Loading agents"
    >
      {Array.from({ length: isMobile ? 3 : 2 }, (_, index) => (
        <div
          key={index}
          className={cn(
            "flex flex-col gap-3 rounded-xl border border-border bg-card p-4",
            isMobile ? "w-full" : "w-[500px] shrink-0"
          )}
        >
          <div className="flex items-center gap-2">
            <Skeleton className="size-2.5 rounded-full" />
            <Skeleton className="h-4 w-40" />
            <Skeleton className="ml-auto h-4 w-12" />
          </div>
          <Skeleton className="h-3 w-24" />
          {!isMobile && <Skeleton className="flex-1 min-h-32" />}
          {isMobile && <Skeleton className="h-16 w-full" />}
        </div>
      ))}
    </div>
  )
}

export function ConversationSkeleton() {
  return (
    <main className="flex-1 flex flex-col bg-background min-w-0 overflow-hidden" aria-label="Loading agent">
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-2.5">
        <Skeleton className="size-4" />
        <Skeleton className="h-4 w-48" />
        <Skeleton className="ml-auto h-4 w-16" />
      </header>
      <div className="flex-1 space-y-6 overflow-hidden p-6">
        <div className="space-y-2">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-4 w-1/2" />
        </div>
        <div className="ml-auto max-w-md space-y-2">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-2/3" />
        </div>
        <div className="space-y-2">
          <Skeleton className="h-4 w-2/3" />
          <Skeleton className="h-4 w-5/6" />
          <Skeleton className="h-4 w-1/3" />
        </div>
      </div>
    </main>
  )
}

/** The whole agents shell, for the first paint before anything is known. */
export function ShellSkeleton() {
  return (
    <div className="flex h-screen-safe bg-background">
      <aside className="hidden md:flex w-[260px] h-screen-safe flex-col border-r border-border bg-card">
        <div className="flex items-center justify-between border-b border-border px-4 py-2.5">
          <Skeleton className="h-6 w-28" />
          <Skeleton className="size-8" />
        </div>
        <SidebarListSkeleton />
      </aside>
      <main className="flex-1 flex flex-col min-w-0 overflow-hidden">
        <header className="flex shrink-0 items-center gap-2 border-b border-border px-3 py-2 md:px-4 md:py-2.5">
          <Skeleton className="size-4" />
          <Skeleton className="h-4 w-16" />
          <Skeleton className="h-3 w-32" />
        </header>
        <div className="flex-1 relative min-h-0">
          <BoardSkeleton />
        </div>
      </main>
    </div>
  )
}
