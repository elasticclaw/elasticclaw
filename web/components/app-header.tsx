"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { Settings } from "lucide-react"
import { PRIMARY_NAV, isPrimaryNavActive } from "@/components/primary-nav"
import { useCapabilities } from "@/hooks/use-capabilities"
import { cn } from "@/lib/utils"

// AppHeader is the single app-level chrome bar. It spans the full viewport
// width above the sidebar and content on every screen, so the primary
// navigation never changes position between routes. Branding intentionally
// stays out of it: the expanded board sidebar already shows the app name.
export function AppHeader() {
  const pathname = usePathname()
  // isAdmin defaults to false while /api/auth/me resolves, so the Settings
  // gear never flashes for non-admins.
  const { isAdmin } = useCapabilities()

  return (
    <header className="flex h-11 shrink-0 items-center justify-between border-b border-border bg-card px-2">
      <nav className="flex items-center gap-1">
        {PRIMARY_NAV.map(({ href, label, icon: Icon }) => (
          <Link
            key={href}
            href={href}
            className={cn(
              "flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm transition-colors",
              isPrimaryNavActive(pathname, href)
                ? "bg-accent text-foreground font-medium"
                : "text-muted-foreground hover:bg-accent hover:text-foreground"
            )}
          >
            <Icon className="size-4" />
            {label}
          </Link>
        ))}
      </nav>
      {isAdmin && (
        <Link
          href="/settings"
          title="Settings"
          className="flex size-8 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <Settings className="size-4" />
        </Link>
      )}
    </header>
  )
}
