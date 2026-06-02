export type ClawStatus = "connected" | "idle" | "offline" | "provisioning" | "error"

export interface Claw {
  id: string
  name: string
  template: string
  status: ClawStatus
  uptime: number // in seconds, computed from created_at
  unreadCount: number
  isStreaming: boolean
  pinned: boolean
  tags: string[]
  color: string // accent color name, e.g. "blue", "emerald"
  contextUsage: number // 0-100 percentage, hardcoded 0 for now
  description?: string
  reason?: string // stop reason when status is error
  bootstrap_status?: string
  // SSH / terminal access
  ssh_host?: string
  ssh_port?: number
  ssh_user?: string
  // API fields
  last_seen?: string
  created_at?: string
  tenant_id?: string
}

export interface Message {
  id: string
  role: "user" | "claw" | "system" | "hub" | "activity"
  content: string
  format?: string // "pre" = preserve whitespace
  timestamp: Date
  activity?: AgentActivity
  // API fields
  claw_id?: string
  tenant_id?: string
}

export interface AgentActivity {
  kind: string
  stream?: string
  phase?: string
  tool?: string
  detail?: string
  command?: string
  path?: string
  url?: string
  message?: string
  error?: string
}

// Raw API types
export interface ApiClaw {
  id: string
  name: string
  template: string
  status: "connected" | "offline" | "provisioning" | "starting" | "error" | "idle"
  last_seen: string
  created_at: string
  tenant_id: string
  context_usage?: number
  tags?: string[]
  color?: string
  ssh_host?: string
  ssh_port?: number
  ssh_user?: string
  bootstrap_status?: string
}

export interface ApiMessage {
  id: string
  claw_id: string
  tenant_id: string
  role: "user" | "claw" | "hub" | "activity"
  content: string
  format?: string
  created_at: string
}

export interface CreateClawRequest {
  name: string
  template: string
  provider: string
  default_model?: string
  files?: string[]
}
