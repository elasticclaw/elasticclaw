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

/**
 * Per-claw accent classes, keyed by the legacy color names the API stores.
 *
 * The Modernist system has no 16-hue palette: it has one accent, a secondary
 * accent, a data hue, three status hues and a neutral ramp. The 16 keys are
 * therefore spread across those families at different ramp steps, which keeps
 * every claw visually distinguishable while never introducing a color the
 * design system does not own. Keys and the { border, bubble, dot, badge }
 * shape are unchanged, so consumers keep working.
 *
 * Every class below must stay a full literal string — Tailwind v4 only emits
 * classes it can find verbatim in the source.
 */
export const COLOR_CLASSES: Record<string, {
  border: string
  bubble: string
  dot: string
  badge: string
}> = {
  slate:   { border: "border-[var(--ds-neutral-500)]",   bubble: "bg-[var(--ds-neutral-500)]/10",   dot: "bg-[var(--ds-neutral-500)]",   badge: "bg-[var(--ds-neutral-500)]/20 text-[var(--ds-neutral-300)]" },
  red:     { border: "border-[var(--ds-accent-600)]",    bubble: "bg-[var(--ds-accent-600)]/10",    dot: "bg-[var(--ds-accent-600)]",    badge: "bg-[var(--ds-accent-600)]/20 text-[var(--ds-accent-300)]" },
  orange:  { border: "border-[var(--ds-accent-500)]",    bubble: "bg-[var(--ds-accent-500)]/10",    dot: "bg-[var(--ds-accent-500)]",    badge: "bg-[var(--ds-accent-500)]/20 text-[var(--ds-accent-300)]" },
  amber:   { border: "border-status-warn",               bubble: "bg-status-warn/10",               dot: "bg-status-warn",               badge: "bg-status-warn/20 text-status-warn" },
  lime:    { border: "border-[var(--ds-accent-2-400)]",  bubble: "bg-[var(--ds-accent-2-400)]/10",  dot: "bg-[var(--ds-accent-2-400)]",  badge: "bg-[var(--ds-accent-2-400)]/20 text-[var(--ds-accent-2-300)]" },
  green:   { border: "border-status-ok",                 bubble: "bg-status-ok/10",                 dot: "bg-status-ok",                 badge: "bg-status-ok/20 text-status-ok" },
  emerald: { border: "border-[var(--ds-accent-2-300)]",  bubble: "bg-[var(--ds-accent-2-300)]/10",  dot: "bg-[var(--ds-accent-2-300)]",  badge: "bg-[var(--ds-accent-2-300)]/20 text-[var(--ds-accent-2-200)]" },
  // The heatmap ramp is reversed in dark mode, so its low steps sit at the
  // card surface's luminance — identity colors must stay on the bright half
  // (the shell ships dark-only).
  teal:    { border: "border-data-light",                bubble: "bg-data-light/10",                dot: "bg-data-light",                badge: "bg-data-light/20 text-data-light" },
  cyan:    { border: "border-heatmap-4",                 bubble: "bg-heatmap-4/10",                 dot: "bg-heatmap-4",                 badge: "bg-heatmap-4/20 text-heatmap-5" },
  sky:     { border: "border-heatmap-5",                 bubble: "bg-heatmap-5/10",                 dot: "bg-heatmap-5",                 badge: "bg-heatmap-5/20 text-heatmap-5" },
  blue:    { border: "border-data",                      bubble: "bg-data/10",                      dot: "bg-data",                      badge: "bg-data/20 text-data-light" },
  indigo:  { border: "border-data-dark",                 bubble: "bg-data-dark/10",                 dot: "bg-data-dark",                 badge: "bg-data-dark/20 text-data-light" },
  violet:  { border: "border-[var(--ds-accent-2-200)]",  bubble: "bg-[var(--ds-accent-2-200)]/10",  dot: "bg-[var(--ds-accent-2-200)]",  badge: "bg-[var(--ds-accent-2-200)]/20 text-[var(--ds-accent-2-200)]" },
  purple:  { border: "border-[var(--ds-accent-2-600)]",  bubble: "bg-[var(--ds-accent-2-600)]/10",  dot: "bg-[var(--ds-accent-2-600)]",  badge: "bg-[var(--ds-accent-2-600)]/20 text-[var(--ds-accent-2-300)]" },
  pink:    { border: "border-[var(--ds-accent-2-500)]",  bubble: "bg-[var(--ds-accent-2-500)]/10",  dot: "bg-[var(--ds-accent-2-500)]",  badge: "bg-[var(--ds-accent-2-500)]/20 text-[var(--ds-accent-2-200)]" },
  rose:    { border: "border-[var(--ds-accent-400)]",    bubble: "bg-[var(--ds-accent-400)]/10",    dot: "bg-[var(--ds-accent-400)]",    badge: "bg-[var(--ds-accent-400)]/20 text-[var(--ds-accent-200)]" },
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
