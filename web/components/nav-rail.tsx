"use client"

import { Fragment, useEffect, useState } from "react"
import Link from "next/link"
import { usePathname, useRouter } from "next/navigation"
import { BarChart3, Bot, ChevronDown, LogOut, User } from "lucide-react"
import {
  LEGACY_ANALYTICS_SECTION,
  SETTINGS_NAV_GROUPS,
  WORKSPACE_SECTIONS,
  activeSettingsSection,
  isValidSection,
} from "@/app/settings/[[...parts]]/sections"
import { useBranding } from "@/hooks/use-branding"
import { useCapabilities } from "@/hooks/use-capabilities"
import { clearConfig, fetchWorkspaces, type Workspace } from "@/lib/api"
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

// Workspace segment of a settings URL, mirroring the parsing in
// settings-content.tsx: /settings/{workspace}[/{section}]. Section names, the
// "_workspace" static-export sentinel and the legacy analytics slug are never
// workspace names. Returns "" when the URL carries no workspace.
function routeWorkspaceOf(pathname: string): string {
  const parts = pathname.split("/").filter(Boolean)
  if (parts[0] !== "settings") return ""
  const first = parts[1] ?? ""
  if (!first || isValidSection(first) || first === "_workspace" || first === LEGACY_ANALYTICS_SECTION) return ""
  return decodeURIComponent(first)
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
// brand, the primary navigation, the admin-only settings section groups and
// the user identity block with sign-out. Everything admin-gated defaults to hidden
// until /api/auth/me resolves, so nothing flashes for non-admins.
export function NavRail() {
  const pathname = usePathname()
  const router = useRouter()
  const { appName } = useBranding()
  const { me, isAdmin } = useCapabilities()

  // Workspace list for the rail's picker. Fetched once per mount, admin-only,
  // and failures are swallowed: the picker degrades to the disabled
  // "No workspaces" state rather than surfacing an error in app chrome.
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    fetchWorkspaces()
      .then((data) => {
        if (!cancelled) setWorkspaces(data)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [isAdmin])

  // The HOME badge mirrors the login landing rule (app/login/page.tsx):
  // admins land on /analytics, everyone else on /. Rendered only once
  // /api/auth/me has resolved so the badge never jumps between items.
  const homeHref = me ? (isAdmin ? "/analytics" : "/") : null

  // Which settings section (if any) the current URL belongs to; drives the
  // active highlight for both /settings/{section} and
  // /settings/{workspace}/{section}.
  const activeSection = activeSettingsSection(pathname)

  // The rail owns no workspace state: the URL is the source of truth. The
  // picker displays the workspace from the URL when present (falling back to
  // the first available, which is what settings-content.tsx resolves to), and
  // changing it only navigates — the normalisation effect in
  // settings-content.tsx handles resolution and redirects from there. It
  // renders only on /settings, where the WORKSPACE group it scopes is in use;
  // on / and /analytics it would be inert.
  const onSettings = pathname.startsWith("/settings")
  const routeWorkspace = routeWorkspaceOf(pathname)
  const pickerWorkspace = routeWorkspace && workspaces.some((workspace) => workspace.name === routeWorkspace)
    ? routeWorkspace
    : workspaces[0]?.name ?? ""
  const pickerInitial = pickerWorkspace ? pickerWorkspace.trim()[0].toUpperCase() : "-"

  const selectWorkspace = (workspace: string) => {
    const base = `/settings/${encodeURIComponent(workspace)}`
    const scoped = activeSection && activeSection !== "workspaces" && WORKSPACE_SECTIONS.has(activeSection)
    router.push(scoped ? `${base}/${activeSection}` : base)
  }

  // Password sessions carry no GitHub identity: /api/auth/me returns only
  // auth_method, is_admin and capabilities. Falling back to login/name would
  // render an empty name and a bare "@" for the local password flow, which is
  // the only way to sign in when GitHub OAuth is not configured.
  const hasGitHubIdentity = Boolean(me?.login)
  const displayName = hasGitHubIdentity ? me?.name || me?.login || "" : "Signed in"
  const identitySubtitle = hasGitHubIdentity
    ? `@${me?.login}${isAdmin ? " · Admin" : ""}`
    : `Password session${isAdmin ? " · Admin" : ""}`

  return (
    <aside className="flex h-full w-[250px] shrink-0 flex-col border-r border-border bg-background">
      <div className="flex items-center gap-2 px-4 pb-2 pt-4">
        <div className="flex size-6 shrink-0 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground">
          {(appName[0] ?? "E").toUpperCase()}
        </div>
        <span className="truncate text-sm font-semibold tracking-tight">{appName}</span>
      </div>

      <nav className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
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
        {/* Settings sections rendered directly in the rail, admin-only, from
            the shared catalogue in app/settings/[[...parts]]/sections.ts. Each
            item links to /settings/{section}; settings-content.tsx normalises
            workspace-scoped sections to /settings/{workspace}/{section}, and
            activeSettingsSection resolves the highlight for both URL shapes. */}
        {/* Workspace picker: sits directly above the WORKSPACE group caption
            because that group is exactly what it scopes. Admin-only (like the
            groups below, hidden until /api/auth/me resolves) and settings-only.
            With zero workspaces it renders disabled as "No workspaces" — a
            deliberate empty state, not an error. */}
        {isAdmin && onSettings && (
          <div className="relative mt-4">
            <div
              className={cn(
                "pointer-events-none absolute left-2.5 top-1/2 z-10 flex size-5 -translate-y-1/2 items-center justify-center rounded text-[11px] font-semibold",
                pickerWorkspace ? "bg-blue-600 text-white shadow-sm" : "bg-accent text-muted-foreground"
              )}
            >
              {pickerInitial}
            </div>
            <select
              aria-label="Workspace"
              value={pickerWorkspace}
              onChange={(event) => selectWorkspace(event.target.value)}
              disabled={workspaces.length === 0}
              className="h-9 w-full appearance-none truncate rounded-lg border border-border bg-transparent pl-9 pr-8 text-sm font-semibold outline-none transition-colors hover:bg-secondary focus:bg-secondary disabled:text-muted-foreground disabled:hover:bg-transparent"
            >
              {workspaces.length === 0 ? (
                <option value="">No workspaces</option>
              ) : (
                workspaces.map((workspace) => (
                  <option key={workspace.name} value={workspace.name}>{workspace.name}</option>
                ))
              )}
            </select>
            <ChevronDown className="pointer-events-none absolute right-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          </div>
        )}
        {isAdmin &&
          SETTINGS_NAV_GROUPS.map((group) => (
            <Fragment key={group.label}>
              <GroupLabel>{group.label}</GroupLabel>
              <div className="space-y-0.5">
                {group.items.map(({ id, label, icon }) => (
                  <NavItem
                    key={id}
                    href={`/settings/${id}`}
                    label={label}
                    icon={icon}
                    active={activeSection === id}
                  />
                ))}
              </div>
            </Fragment>
          ))}
      </nav>

      <div className="border-t border-border p-3">
        {me && (
          <div className="flex items-center gap-2.5 px-1 pb-2">
            {hasGitHubIdentity && me.avatar_url ? (
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
                {hasGitHubIdentity ? initialsOf(displayName) : <User className="size-4" />}
              </div>
            )}
            <div className="min-w-0">
              <p className="truncate text-sm font-medium">{displayName}</p>
              <p className="truncate text-xs text-muted-foreground">{identitySubtitle}</p>
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
