import type { ApiClaw, ApiMessage, Claw, Message, ClawStatus } from "./types"

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
 * Compute uptime in seconds from created_at if status is connected.
 */
export function computeUptime(apiClaw: ApiClaw): number {
  if (apiClaw.status !== "connected") return 0
  try {
    const created = new Date(apiClaw.created_at).getTime()
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
    tags: overrides.tags ?? parseTagsFromName(apiClaw.name),
    contextUsage: overrides.contextUsage ?? apiClaw.context_usage ?? 0,
    description: overrides.description,
    ssh_host: apiClaw.ssh_host,
    ssh_port: apiClaw.ssh_port,
    ssh_user: apiClaw.ssh_user,
    last_seen: apiClaw.last_seen,
    created_at: apiClaw.created_at,
    tenant_id: apiClaw.tenant_id,
  }
}

export function mapApiMessage(apiMsg: ApiMessage): Message {
  return {
    id: apiMsg.id,
    role: apiMsg.role,
    content: apiMsg.content,
    timestamp: new Date(apiMsg.created_at),
    claw_id: apiMsg.claw_id,
    tenant_id: apiMsg.tenant_id,
  }
}
