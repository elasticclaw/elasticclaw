"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { LogOut, Settings } from "lucide-react"
import { PRIMARY_NAV, isPrimaryNavActive } from "@/components/primary-nav"
import { useCapabilities } from "@/hooks/use-capabilities"
import { clearConfig } from "@/lib/api"
import { getAuthToken } from "@/lib/auth-storage"
import { cn } from "@/lib/utils"

async function signOut() {
  // Best-effort server-side logout: an unreachable hub must never trap the
  // user in a session, so failures are ignored and local state is cleared
  // regardless.
  const token = getAuthToken() || ""
  if (token) {
    const { getHubUrl } = await import("@/lib/hub-url")
    const hubUrl = getHubUrl()
    const logoutUrl = hubUrl ? `${hubUrl}/api/auth/logout` : "/api/auth/logout"
    await fetch(logoutUrl, { method: "POST", headers: { Authorization: `Bearer ${token}` } }).catch(() => {})
  }
  clearConfig()
  window.location.href = "/login"
}

// AppHeader is the single app-level chrome bar. It spans the full viewport
// width above the sidebar and content on every screen, so the primary
// navigation never changes position between routes. Branding intentionally
// stays out of it: the expanded board sidebar already shows the app name.
export function AppHeader() {
  const pathname = usePathname()
  // isAdmin defaults to false while /api/auth/me resolves, so the Settings
  // link never flashes for non-admins.
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
      <div className="flex items-center gap-1">
        {isAdmin && (
          <Link
            href="/settings"
            className={cn(
              "flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm transition-colors",
              isPrimaryNavActive(pathname, "/settings")
                ? "bg-accent text-foreground font-medium"
                : "text-muted-foreground hover:bg-accent hover:text-foreground"
            )}
          >
            <Settings className="size-4" />
            Settings
          </Link>
        )}
        {/* Divider keeps the sign-out action visually apart from navigation:
            Settings is a destination, Sign out ends the session. */}
        <div className="mx-1 h-4 w-px bg-border" aria-hidden="true" />
        <button
          type="button"
          onClick={signOut}
          className="flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm text-muted-foreground/80 transition-colors hover:bg-accent hover:text-foreground"
        >
          <LogOut className="size-4" />
          Sign out
        </button>
      </div>
    </header>
  )
}
