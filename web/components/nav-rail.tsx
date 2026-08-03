"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { BarChart3, Bot, LogOut, Settings } from "lucide-react"
import { useBranding } from "@/hooks/use-branding"
import { useCapabilities } from "@/hooks/use-capabilities"
import { clearConfig } from "@/lib/api"
import { getAuthToken } from "@/lib/auth-storage"
import { cn } from "@/lib/utils"

// Primary routes shown in the WORK group of the rail.
const PRIMARY_NAV = [
  { href: "/", label: "Agents", icon: Bot },
  { href: "/analytics", label: "Analytics", icon: BarChart3 },
] as const

function isNavActive(pathname: string, href: string) {
  return href === "/" ? pathname === "/" : pathname.startsWith(href)
}

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

function GroupLabel({ children }: { children: React.ReactNode }) {
  return (
    <p className="px-2 pb-1 pt-4 text-[10px] font-semibold uppercase tracking-widest text-muted-foreground/70">
      {children}
    </p>
  )
}

function NavItem({
  href,
  label,
  icon: Icon,
  active,
  home,
}: {
  href: string
  label: string
  icon: typeof Bot
  active: boolean
  home?: boolean
}) {
  return (
    <Link
      href={href}
      className={cn(
        "flex items-center gap-2 rounded-md px-2 py-1.5 text-sm transition-colors",
        active
          ? "bg-accent font-medium text-foreground"
          : "text-muted-foreground hover:bg-accent hover:text-foreground"
      )}
    >
      <Icon className="size-4 shrink-0" />
      <span className="truncate">{label}</span>
      {home && (
        <span className="ml-auto rounded border border-border px-1 py-px text-[9px] font-semibold uppercase tracking-wider text-muted-foreground">
          Home
        </span>
      )}
    </Link>
  )
}

function initialsOf(name: string) {
  const parts = name.trim().split(/\s+/).filter(Boolean)
  if (parts.length === 0) return "?"
  const first = parts[0][0] ?? ""
  const last = parts.length > 1 ? parts[parts.length - 1][0] ?? "" : ""
  return (first + last).toUpperCase()
}

// NavRail is the single piece of app-level chrome: a full-height left rail
// mounted on every primary screen (/, /analytics, /settings). It carries the
// brand, the primary navigation, the admin-only CONFIGURE group and the user
// identity block with sign-out. Everything admin-gated defaults to hidden
// until /api/auth/me resolves, so nothing flashes for non-admins.
export function NavRail() {
  const pathname = usePathname()
  const { appName } = useBranding()
  const { me, isAdmin } = useCapabilities()

  // The HOME badge mirrors the login landing rule (app/login/page.tsx):
  // admins land on /analytics, everyone else on /. Rendered only once
  // /api/auth/me has resolved so the badge never jumps between items.
  const homeHref = me ? (isAdmin ? "/analytics" : "/") : null
  const displayName = me?.name || me?.login || ""

  return (
    <aside className="flex h-full w-52 shrink-0 flex-col border-r border-border bg-background">
      <div className="flex items-center gap-2 px-4 pb-2 pt-4">
        <div className="flex size-6 shrink-0 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground">
          {(appName[0] ?? "E").toUpperCase()}
        </div>
        <span className="truncate text-sm font-semibold tracking-tight">{appName}</span>
      </div>

      <nav className="min-h-0 flex-1 overflow-y-auto px-2">
        <GroupLabel>Work</GroupLabel>
        <div className="space-y-0.5">
          {PRIMARY_NAV.map(({ href, label, icon }) => (
            <NavItem
              key={href}
              href={href}
              label={label}
              icon={icon}
              active={isNavActive(pathname, href)}
              home={homeHref === href}
            />
          ))}
        </div>
        {isAdmin && (
          <>
            <GroupLabel>Configure</GroupLabel>
            <div className="space-y-0.5">
              <NavItem
                href="/settings"
                label="Settings"
                icon={Settings}
                active={isNavActive(pathname, "/settings")}
              />
            </div>
          </>
        )}
      </nav>

      <div className="border-t border-border p-3">
        {me && (
          <div className="flex items-center gap-2.5 px-1 pb-2">
            {me.avatar_url ? (
              /* Static export cannot use the Next image optimizer; avatars
                 come from an arbitrary hub-configured host. */
              // eslint-disable-next-line @next/next/no-img-element
              <img
                src={me.avatar_url}
                alt=""
                className="size-8 shrink-0 rounded-full border border-border"
              />
            ) : (
              <div className="flex size-8 shrink-0 items-center justify-center rounded-full bg-accent text-xs font-medium text-foreground">
                {initialsOf(displayName)}
              </div>
            )}
            <div className="min-w-0">
              <p className="truncate text-sm font-medium">{displayName}</p>
              <p className="truncate text-xs text-muted-foreground">
                @{me.login}
                {isAdmin && " · Admin"}
              </p>
            </div>
          </div>
        )}
        <button
          type="button"
          onClick={signOut}
          className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-sm text-muted-foreground/80 transition-colors hover:bg-accent hover:text-foreground"
        >
          <LogOut className="size-4" />
          Sign out
        </button>
      </div>
    </aside>
  )
}
