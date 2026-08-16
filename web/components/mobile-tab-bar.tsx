"use client"

import { BarChart3, LayoutGrid, Settings } from "lucide-react"
import { cn } from "@/lib/utils"
import type { HomeView } from "@/components/home-shell"

/**
 * Fixed bottom tab bar for phones. Replaces the sidebar footer destinations:
 * Agents always, Analytics and Settings for admins. Sign out lives in the
 * board header overflow menu, not here. Padded above the home indicator via
 * the safe-area inset (non-zero thanks to viewportFit: 'cover').
 */
export function MobileTabBar({
  view,
  isAdmin,
  onSelectAgents,
  onSelectAnalytics,
}: {
  view: HomeView
  isAdmin: boolean
  onSelectAgents: () => void
  onSelectAnalytics: () => void
}) {
  return (
    <nav className="shrink-0 border-t-2 border-border bg-card pb-[env(safe-area-inset-bottom)]">
      <div className="flex">
        <TabItem
          label="Agents"
          icon={LayoutGrid}
          active={view === "agents"}
          onClick={onSelectAgents}
        />
        {isAdmin && (
          <>
            <TabItem
              label="Analytics"
              icon={BarChart3}
              active={view === "analytics"}
              onClick={onSelectAnalytics}
            />
            <TabItem
              label="Settings"
              icon={Settings}
              active={false}
              onClick={() => { window.location.href = "/settings" }}
            />
          </>
        )}
      </div>
    </nav>
  )
}

function TabItem({
  label,
  icon: Icon,
  active,
  onClick,
}: {
  label: string
  icon: typeof LayoutGrid
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        // Matches the design system's nav: the current destination is the only
        // thing wearing the accent, everything else is quiet.
        "flex min-h-12 flex-1 flex-col items-center justify-center gap-0.5 py-1.5 kicker font-semibold transition-colors",
        active ? "text-primary" : "text-muted-foreground hover:text-foreground"
      )}
      aria-current={active ? "page" : undefined}
    >
      <Icon className="size-5" />
      {label}
    </button>
  )
}
