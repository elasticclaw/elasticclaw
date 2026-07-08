import type { components } from "./gen/api"

// ─── Generated wire types (single source: api/openapi.yaml) ─────────────────
// Raw API types are derived from the generated OpenAPI contract. Regenerate
// with `make gen`. Presentation types (Claw, Message, ...) stay hand-written
// and are produced from the wire types by lib/mappers.ts.

/** Wire status enum as emitted by the hub (API + WebSocket). */
export type ApiClawStatus = components["schemas"]["ClawStatus"]

export type ApiClaw = components["schemas"]["Claw"]
export type ApiMessage = components["schemas"]["Message"]
export type CreateClawRequest = components["schemas"]["CreateClawRequest"]

export type TaskRunAnalyticsSummary = components["schemas"]["TaskRunAnalyticsSummary"]
export type TaskRunSummary = components["schemas"]["TaskRunSummary"]
export type TaskRunsResponse = components["schemas"]["TaskRunsResponse"]
export type TaskRunAttempt = components["schemas"]["TaskRunAttempt"]
export type TaskRunEvent = components["schemas"]["TaskRunEvent"]
export type TaskRunPR = components["schemas"]["TaskRunPR"]
export type TaskRunFilterOptions = components["schemas"]["TaskRunFilterOptions"]

// ─── Presentation types ──────────────────────────────────────────────────────

/**
 * UI status of a claw. Narrower than ApiClawStatus: pending/provisioning/
 * starting all render as "provisioning" (spinner) and deleted claws are
 * filtered out of the list. See mapApiStatus in lib/mappers.ts.
 */
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
  githubIssueId?: string
  githubIssueUrl?: string
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
  role: "user" | "claw" | "system" | "hub" | "activity" | "activity_summary"
  content: string
  format?: string // "pre" = preserve whitespace
  timestamp: Date
  activity?: AgentActivity
  activitySummary?: ActivitySummary
  // API fields
  claw_id?: string
  tenant_id?: string
}

export interface ActivitySummary {
  count: number
  from?: string
  to?: string
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

export interface TaskRunAnalyticsFilters {
  status?: string
  ownerType?: string
  workspace?: string
  workflow?: string
  factory?: string
  integration?: string
  repo?: string
  model?: string
  warningType?: string
  failureType?: string
  humanTouched?: boolean
  mergedPrs?: boolean
  analyticsEnabled?: boolean
  requiresPr?: boolean
  from?: string
  to?: string
  limit?: number
  cursor?: string
}

export type DependencyStatusValue = "operational" | "degraded" | "downtime" | "unknown"

export enum DependencyKind {
  Model = "model",
  Sandbox = "sandbox",
  IssueTracker = "issue_tracker",
}

export interface DependencyStatus {
  id: string
  name: string
  kind: DependencyKind
  status: DependencyStatusValue
  message?: string
  source?: string
  checkedAt: string
}

export interface DependencyStatusResponse {
  dependencies: DependencyStatus[]
  downtimeCount: number
  checkedAt: string
}
