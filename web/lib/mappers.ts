import type { ApiClaw, ApiMessage, Claw, Message, ClawStatus } from "./types"

export const CLAW_COLORS = [
  "slate", "red", "orange", "amber", "lime", "green", "emerald", "teal",
  "cyan", "sky", "blue", "indigo", "violet", "purple", "pink", "rose",
] as const

export type ClawColor = typeof CLAW_COLORS[number]

// Auto-assign a color from the claw name (deterministic)
function autoColor(name: string): string {
  let h = 0
  for (let i = 0; i < name.length; i++) {
    h = (h * 31 + name.charCodeAt(i)) >>> 0
  }
  return CLAW_COLORS[h % CLAW_COLORS.length]
}

// Tailwind color classes for each named color
// All must be in the safelist so Tailwind includes them
export const COLOR_CLASSES: Record<string, {
  border: string
  bubble: string
  dot: string
  badge: string
}> = {
  slate:   { border: "border-slate-500",   bubble: "bg-slate-500/10",   dot: "bg-slate-400",   badge: "bg-slate-500/20 text-slate-300" },
  red:     { border: "border-red-500",     bubble: "bg-red-500/10",     dot: "bg-red-400",     badge: "bg-red-500/20 text-red-300" },
  orange:  { border: "border-orange-500",  bubble: "bg-orange-500/10",  dot: "bg-orange-400",  badge: "bg-orange-500/20 text-orange-300" },
  amber:   { border: "border-amber-500",   bubble: "bg-amber-500/10",   dot: "bg-amber-400",   badge: "bg-amber-500/20 text-amber-300" },
  lime:    { border: "border-lime-500",    bubble: "bg-lime-500/10",    dot: "bg-lime-400",    badge: "bg-lime-500/20 text-lime-300" },
  green:   { border: "border-green-500",   bubble: "bg-green-500/10",   dot: "bg-green-400",   badge: "bg-green-500/20 text-green-300" },
  emerald: { border: "border-emerald-500", bubble: "bg-emerald-500/10", dot: "bg-emerald-400", badge: "bg-emerald-500/20 text-emerald-300" },
  teal:    { border: "border-teal-500",    bubble: "bg-teal-500/10",    dot: "bg-teal-400",    badge: "bg-teal-500/20 text-teal-300" },
  cyan:    { border: "border-cyan-500",    bubble: "bg-cyan-500/10",    dot: "bg-cyan-400",    badge: "bg-cyan-500/20 text-cyan-300" },
  sky:     { border: "border-sky-500",     bubble: "bg-sky-500/10",     dot: "bg-sky-400",     badge: "bg-sky-500/20 text-sky-300" },
  blue:    { border: "border-blue-500",    bubble: "bg-blue-500/10",    dot: "bg-blue-400",    badge: "bg-blue-500/20 text-blue-300" },
  indigo:  { border: "border-indigo-500",  bubble: "bg-indigo-500/10",  dot: "bg-indigo-400",  badge: "bg-indigo-500/20 text-indigo-300" },
  violet:  { border: "border-violet-500",  bubble: "bg-violet-500/10",  dot: "bg-violet-400",  badge: "bg-violet-500/20 text-violet-300" },
  purple:  { border: "border-purple-500",  bubble: "bg-purple-500/10",  dot: "bg-purple-400",  badge: "bg-purple-500/20 text-purple-300" },
  pink:    { border: "border-pink-500",    bubble: "bg-pink-500/10",    dot: "bg-pink-400",    badge: "bg-pink-500/20 text-pink-300" },
  rose:    { border: "border-rose-500",    bubble: "bg-rose-500/10",    dot: "bg-rose-400",    badge: "bg-rose-500/20 text-rose-300" },
}

/**
 * Parse tags from claw name: "name env=prod key=val" → { env: "prod", key: "val" }
 */
export function parseTagsFromName(name: string): Record<string, string> {
  const tags: Record<string, string> = {}
  const parts = name.split(/\s+/)
  for (const part of parts) {
    const eq = part.indexOf("=")
    if (eq > 0) {
      const key = part.slice(0, eq)
      const value = part.slice(eq + 1)
      if (key && value) tags[key] = value
    }
  }
  return tags
}

/**
 * Compute uptime in seconds from created_at if status is connected or idle.
 */
export function computeUptime(apiClaw: ApiClaw): number {
  if (apiClaw.status !== "connected" && apiClaw.status !== "idle") return 0
  try {
    const created = new Date(apiClaw.created_at).getTime()
    if (!Number.isFinite(created)) return 0
    const now = Date.now()
    return Math.max(0, Math.floor((now - created) / 1000))
  } catch {
    return 0
  }
}

export function mapApiStatus(status: ApiClaw["status"]): ClawStatus {
  switch (status) {
    case "connected":
      return "connected"
    case "idle":
      return "idle"
    case "preview":
      return "preview"
    case "provisioning":
    case "starting":
      return "provisioning"
    case "error":
      return "error"
    default:
      return "offline"
  }
}

export function mapApiClaw(
  apiClaw: ApiClaw,
  overrides: Partial<Claw> = {}
): Claw {
  return {
    id: apiClaw.id,
    name: apiClaw.name,
    template: apiClaw.template,
    status: overrides.status ?? mapApiStatus(apiClaw.status),
    uptime: overrides.uptime ?? computeUptime(apiClaw),
    unreadCount: overrides.unreadCount ?? 0,
    isStreaming: overrides.isStreaming ?? false,
    pinned: overrides.pinned ?? false,
    tags: overrides.tags ?? (apiClaw.tags ?? []),
    color: overrides.color ?? (apiClaw.color || autoColor(apiClaw.name)),
    contextUsage: overrides.contextUsage ?? apiClaw.context_usage ?? 0,
    description: overrides.description,
    reason: overrides.reason,
    bootstrap_status: overrides.bootstrap_status ?? apiClaw.bootstrap_status,
    githubIssueId: overrides.githubIssueId ?? apiClaw.github_issue_id,
    githubIssueUrl: overrides.githubIssueUrl ?? apiClaw.github_issue_url,
    previewPort: overrides.previewPort ?? apiClaw.preview_port,
    previewUrl: overrides.previewUrl ?? apiClaw.preview_url,
    previewLabel: overrides.previewLabel ?? apiClaw.preview_label,
    previewReady: overrides.previewReady ?? apiClaw.preview_ready,
    previewExpiresAt: overrides.previewExpiresAt ?? apiClaw.preview_expires_at,
    ssh_host: apiClaw.ssh_host,
    ssh_port: apiClaw.ssh_port,
    ssh_user: apiClaw.ssh_user,
    last_seen: apiClaw.last_seen,
    created_at: apiClaw.created_at,
    tenant_id: apiClaw.tenant_id,
  }
}

export function mapApiMessage(apiMsg: ApiMessage): Message {
  let activity
  let activitySummary
  if (apiMsg.role === "activity" && apiMsg.format?.startsWith("activity:")) {
    try {
      activity = JSON.parse(apiMsg.format.slice("activity:".length))
    } catch {}
  }
  if (apiMsg.role === "activity_summary" && apiMsg.format?.startsWith("activity_summary:")) {
    try {
      activitySummary = JSON.parse(apiMsg.format.slice("activity_summary:".length))
    } catch {}
  }
  return {
    id: apiMsg.id,
    role: apiMsg.role,
    content: apiMsg.content,
    format: apiMsg.format?.startsWith("activity:") || apiMsg.format?.startsWith("activity_summary:") ? undefined : apiMsg.format,
    activity,
    activitySummary,
    timestamp: new Date(apiMsg.created_at),
    claw_id: apiMsg.claw_id,
    tenant_id: apiMsg.tenant_id,
  }
}
