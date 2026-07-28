import type {
  ApiClaw,
  ApiMessage,
  AnalyticsCostDriver,
  AnalyticsEffectiveness,
  CostOverview,
  CreateClawRequest,
  DependencyStatusResponse,
  GeneralStats,
  TaskRunAnalyticsFilters,
  TaskRunAnalyticsSummary,
  TaskRunAttempt,
  TaskRunEvent,
  TaskRunFilterOptions,
  TaskRunOutput,
  TaskRunPR,
  TaskRunsResponse,
  TaskRunSummary,
  WorkflowRunsResponse,
} from "./types"
import { getHubUrl, setHubUrl } from "./hub-url"
import { getAuthToken, requestAuthToken, clearAuthTokens } from "./auth-storage"

// Token is resolved once and cached.
// Priority: sessionStorage or an open-tab session sync > /api/hub-config (proxy to hub)
let _token: string | null = null
let _tokenPromise: Promise<string> | null = null

export async function resolveToken(): Promise<string> {
  if (_token) return Promise.resolve(_token)
  if (_tokenPromise) return _tokenPromise

  if (typeof window === "undefined") {
    // Server-side: no token (hub is the authority)
    _token = ""
    return Promise.resolve(_token)
  }

  // Check this tab first, then ask other currently open tabs for their session.
  // Prefer GitHub OAuth token over hub token when both exist
  const authToken = getAuthToken() || await requestAuthToken()
  if (authToken) {
    _token = authToken
    return Promise.resolve(_token)
  }

  // Fall back to hub-config proxy (supports dev mode with Next.js)
  const hubUrl = getHubUrl()
  const hubConfigUrl = hubUrl ? `${hubUrl}/api/hub-config` : "/api/hub-config"

  _tokenPromise = fetch(hubConfigUrl, {
    headers: { Authorization: "Bearer " }
  })
    .then((r) => r.json())
    .then((d) => {
      if (typeof d.hubUrl === "string" && d.hubUrl.trim()) {
        setHubUrl(d.hubUrl.trim())
      }
      _token = d.token || ""
      return _token!
    })
    .catch(() => {
      _token = ""
      return ""
    })

  return _tokenPromise
}

// Sync getter — returns cached token or empty string; call resolveToken() first
function getTokenSync(): string {
  if (_token) return _token
  if (typeof window !== "undefined") {
    return getAuthToken() || ""
  }
  return ""
}

// Pre-fetch token on module load (client-side)
if (typeof window !== "undefined") {
  resolveToken()
}

export class ApiError extends Error {
  constructor(message: string, readonly status: number) {
    super(message)
    this.name = "ApiError"
  }
}

async function apiFetch<T>(path: string, options?: RequestInit): Promise<T> {
  const token = await resolveToken()
  const hubBase = getHubUrl()
  const url = hubBase ? `${hubBase}${path}` : path
  const res = await fetch(url, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
      ...options?.headers,
    },
  })
  if (res.status === 401) {
    return handleSessionExpired<T>()
  }
  if (!res.ok) {
    const body = await res.text()
    let message = body
    try {
      const parsed = JSON.parse(body)
      if (typeof parsed.message === "string") message = parsed.message
      if (typeof parsed.error === "string") message = parsed.error
    } catch { /* not JSON */ }
    if (!message) message = `API error ${res.status}`
    throw new ApiError(message, res.status)
  }
  if (res.status === 204) return undefined as T
  return res.json()
}

function handleSessionExpired<T>(): Promise<T> {
  clearConfig()
  if (typeof window !== "undefined" && !window.location.pathname.startsWith("/login")) {
    window.location.href = "/login"
    // Return a never-resolving promise to prevent the error from propagating
    // and triggering the "Cannot reach hub" error screen before navigation completes
    return new Promise(() => {})
  }
  return Promise.reject(new Error("session expired"))
}

export async function fetchClaws(): Promise<ApiClaw[]> {
  return apiFetch<ApiClaw[]>("/api/claws")
}

export async function fetchMessages(clawId: string, opts?: { before?: string; after?: string }): Promise<ApiMessage[]> {
  const params = new URLSearchParams()
  if (opts?.before) params.set('before', opts.before)
  if (opts?.after) params.set('after', opts.after)
  const qs = params.toString() ? '?' + params.toString() : ''
  return apiFetch<ApiMessage[]>(`/api/messages/${clawId}${qs}`)
}

export async function fetchMessageTimeline(clawId: string, opts?: { before?: string; limit?: number }): Promise<ApiMessage[]> {
  const params = new URLSearchParams()
  if (opts?.before) params.set('before', opts.before)
  if (opts?.limit) params.set('limit', String(opts.limit))
  const qs = params.toString() ? '?' + params.toString() : ''
  return apiFetch<ApiMessage[]>(`/api/messages/${clawId}/timeline${qs}`)
}

export async function fetchActivityMessages(clawId: string, opts: { from?: string; to?: string; before?: string; limit?: number; order?: "asc" | "desc" }): Promise<ApiMessage[]> {
  const params = new URLSearchParams()
  if (opts.from) params.set('from', opts.from)
  if (opts.to) params.set('to', opts.to)
  if (opts.before) params.set('before', opts.before)
  if (opts.limit) params.set('limit', String(opts.limit))
  if (opts.order) params.set('order', opts.order)
  const qs = params.toString() ? '?' + params.toString() : ''
  return apiFetch<ApiMessage[]>(`/api/messages/${clawId}/activity${qs}`)
}


export async function sendMessage(clawId: string, content: string): Promise<ApiMessage> {
  return apiFetch<ApiMessage>(`/api/messages/${clawId}`, {
    method: "POST",
    body: JSON.stringify({ content }),
  })
}

export interface UploadedAttachment {
  name: string
  path: string
  size: number
  mimetype: string
}

// getFileViewUrl returns the hub URL that serves the bytes of an uploaded
// file back to the browser. Suitable for <img src>. Auth is via ?token query
// since browsers can't set Authorization on <img>.
export function getFileViewUrl(clawId: string, path: string): string {
  const token = getTokenSync()
  const hubBase = getHubUrl()
  const base = hubBase ? `${hubBase}/api/files/view/${clawId}` : `/api/files/view/${clawId}`
  const qs = new URLSearchParams({ path, token }).toString()
  return `${base}?${qs}`
}

export async function uploadFiles(clawId: string, files: File[]): Promise<UploadedAttachment[]> {
  const token = await resolveToken()
  const hubBase = getHubUrl()
  const url = hubBase ? `${hubBase}/api/files/${clawId}` : `/api/files/${clawId}`
  const form = new FormData()
  for (const f of files) form.append("files", f, f.name)
  const res = await fetch(url, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
    body: form,
  })
  if (res.status === 401) {
    return handleSessionExpired<UploadedAttachment[]>()
  }
  if (!res.ok) {
    throw new Error(`upload failed ${res.status}: ${await res.text()}`)
  }
  const data = await res.json()
  return data.files as UploadedAttachment[]
}

export async function createClaw(req: CreateClawRequest): Promise<ApiClaw> {
  return apiFetch<ApiClaw>("/api/claws", {
    method: "POST",
    body: JSON.stringify(req),
  })
}

export async function killClaw(id: string): Promise<void> {
  return apiFetch<void>(`/api/claws/${id}`, { method: "DELETE" })
}

export function getHubWsUrl(): string {
  const token = getTokenSync()
  const hub = getHubUrl() || window.location.origin
  const wsBase = hub.replace(/^https:/, "wss:").replace(/^http:/, "ws:")
  return `${wsBase}/api/ws?token=${encodeURIComponent(token)}`
}

export function getTerminalWsUrl(clawId: string): string {
  const token = getTokenSync()
  const hub = getHubUrl() || window.location.origin
  const wsBase = hub.replace(/^https:/, "wss:").replace(/^http:/, "ws:")
  return `${wsBase}/api/terminal/${clawId}?token=${encodeURIComponent(token)}`
}

export function isConfigured(): boolean {
  // With server-side auth, always considered configured
  return true
}

export function saveConfig(_hubUrl: string, _token: string) {
  // No-op — config is server-side
}

export function clearConfig() {
  _token = null
  _tokenPromise = null
  clearAuthTokens()
}

export function getConfig() {
  return {
    hubUrl: "",
    token: getTokenSync(),
  }
}

export async function patchClaw(clawId: string, patch: { name?: string; tags?: string[]; color?: string }): Promise<void> {
  await apiFetch(`/api/claws/${clawId}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  })
}

export interface ClawPR {
  id: string
  repo: string
  prNumber: number
  url: string
  createdAt: string
}

export async function fetchClawPRs(clawId: string): Promise<ClawPR[]> {
  return apiFetch<ClawPR[]>(`/api/claws/${clawId}/prs`)
}

export async function fetchDependencyStatus(): Promise<DependencyStatusResponse> {
  return apiFetch<DependencyStatusResponse>("/api/dependencies/status")
}

export interface WorkflowInput {
  name: string
  type: string
  required?: boolean
  default?: string
  description?: string
  options?: string[]
  validation?: string
  min?: number
  max?: number
}

export interface RepositoryAccess {
  repo: string
  permissions?: string
}

export interface WorkspaceAccess {
  repositories?: RepositoryAccess[]
  env?: string[]
  secrets?: string[]
  webhookSecrets?: string[]
}

export interface Workflow {
  name: string
  workspaceName: string
  source: string
  integration: string
  integrationWorkspace?: string
  triggerStatus?: string
  doneStatus?: string
  projects?: string[]
  labels?: string[]
  assignedTo?: string
  enabled: boolean
  hasWebhookSecret: boolean
  webhookSecretRef?: string
  pipelineYAML?: string
  enableManualTrigger?: boolean
  secretRefs?: Record<string, string>
  inputs?: WorkflowInput[]
}

export interface Workspace {
  name: string
  source: string
  config?: string
  access: WorkspaceAccess
  workflows: Workflow[]
}

export async function fetchWorkspaces(): Promise<Workspace[]> {
  return apiFetch<Workspace[]>("/api/workspaces")
}

export async function fetchWorkflows(workspaceName: string): Promise<Workflow[]> {
  return apiFetch<Workflow[]>(`/api/workspaces/${encodeURIComponent(workspaceName)}/workflows`)
}

export async function fetchWorkflow(workspaceName: string, workflowName: string): Promise<Workflow> {
  return apiFetch<Workflow>(`/api/workspaces/${encodeURIComponent(workspaceName)}/workflows/${encodeURIComponent(workflowName)}`)
}

export async function updateWorkflowControls(
  workflow: Workflow,
  patch: { enabled?: boolean; enableManualTrigger?: boolean }
): Promise<Workflow> {
  return apiFetch<Workflow>(
    `/api/workspaces/${encodeURIComponent(workflow.workspaceName)}/workflows/${encodeURIComponent(workflow.name)}`,
    {
      method: "PATCH",
      body: JSON.stringify(patch),
    }
  )
}

export async function triggerWorkflow(workflow: Workflow, inputs?: Record<string, unknown>): Promise<{ claw_id: string; status: string }> {
  return apiFetch<{ claw_id: string; status: string }>(
    `/api/workspaces/${encodeURIComponent(workflow.workspaceName)}/workflows/${encodeURIComponent(workflow.name)}/trigger`,
    {
      method: "POST",
      body: JSON.stringify({ inputs: inputs || {} }),
    }
  )
}

export async function fetchCronWorkflowRuns(workspaceName: string, workflowName: string, limit = 50): Promise<WorkflowRunsResponse> {
  return apiFetch<WorkflowRunsResponse>(
    `/api/workspaces/${encodeURIComponent(workspaceName)}/workflows/${encodeURIComponent(workflowName)}/cron/runs?limit=${limit}`
  )
}

function taskRunAnalyticsQuery(filters?: TaskRunAnalyticsFilters): string {
  const params = new URLSearchParams()
  if (!filters) return ""
  const entries: Array<[string, string | number | boolean | undefined]> = [
    ["status", filters.status],
    ["ownerType", filters.ownerType],
    ["workspace", filters.workspace],
    ["workflow", filters.workflow],
    ["factory", filters.factory],
    ["integration", filters.integration],
    ["repo", filters.repo],
    ["model", filters.model],
    ["warningType", filters.warningType],
    ["failureType", filters.failureType],
    ["humanTouched", filters.humanTouched],
    ["mergedPrs", filters.mergedPrs],
    ["analyticsEnabled", filters.analyticsEnabled],
    ["requiresPr", filters.requiresPr],
    ["from", filters.from],
    ["to", filters.to],
    ["limit", filters.limit],
    ["cursor", filters.cursor],
  ]
  for (const [key, value] of entries) {
    if (value === undefined || value === null || value === "") continue
    params.set(key, String(value))
  }
  const query = params.toString()
  return query ? `?${query}` : ""
}

function viewerTimezoneQuery(): string {
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone
    return tz ? `tz=${encodeURIComponent(tz)}` : ""
  } catch {
    return ""
  }
}

function appendViewerTimezone(query: string): string {
  const tz = viewerTimezoneQuery()
  if (!tz) return query
  return `${query}${query ? "&" : "?"}${tz}`
}

type AnalyticsRequestOptions = Pick<RequestInit, "signal">

export async function fetchTaskRunAnalyticsSummary(filters?: TaskRunAnalyticsFilters, options?: AnalyticsRequestOptions): Promise<TaskRunAnalyticsSummary> {
  return apiFetch<TaskRunAnalyticsSummary>(`/api/analytics/summary${taskRunAnalyticsQuery(filters)}`, options)
}

export async function fetchCostOverview(filters?: TaskRunAnalyticsFilters): Promise<CostOverview> {
  return apiFetch<CostOverview>(`/api/analytics/costs${taskRunAnalyticsQuery(filters)}`)
}

export async function fetchAnalyticsCosts(filters?: TaskRunAnalyticsFilters, days = 30, groupBy?: "model", scope?: "ledger", options?: AnalyticsRequestOptions): Promise<CostOverview> {
  const query = taskRunAnalyticsQuery(filters)
  const params = new URLSearchParams(query.slice(1))
  params.set("days", String(days))
  if (groupBy) params.set("groupBy", groupBy)
  if (scope) params.set("scope", scope)
  return apiFetch<CostOverview>(`/api/analytics/costs?${params}`, options)
}

export async function fetchAnalyticsEffectiveness(filters?: TaskRunAnalyticsFilters, options?: AnalyticsRequestOptions): Promise<AnalyticsEffectiveness> {
  return apiFetch<AnalyticsEffectiveness>(`/api/analytics/effectiveness${taskRunAnalyticsQuery(filters)}`, options)
}

export async function fetchAnalyticsCostDrivers(filters?: TaskRunAnalyticsFilters, groupBy: "factory" | "workflow" = "factory", options?: AnalyticsRequestOptions): Promise<AnalyticsCostDriver[]> {
  const query = taskRunAnalyticsQuery(filters)
  return apiFetch<AnalyticsCostDriver[]>(`/api/analytics/cost-drivers${query}${query ? "&" : "?"}groupBy=${groupBy}`, options)
}

export async function fetchGeneralStats(filters?: TaskRunAnalyticsFilters, options?: AnalyticsRequestOptions): Promise<GeneralStats> {
  return apiFetch<GeneralStats>(`/api/analytics/general-stats${appendViewerTimezone(taskRunAnalyticsQuery(filters))}`, options)
}

export async function fetchTaskRuns(filters?: TaskRunAnalyticsFilters, options?: AnalyticsRequestOptions): Promise<TaskRunsResponse> {
  return apiFetch<TaskRunsResponse>(`/api/analytics/runs${appendViewerTimezone(taskRunAnalyticsQuery(filters))}`, options)
}

export async function fetchTaskRun(runId: string): Promise<{ run: TaskRunSummary }> {
  return apiFetch<{ run: TaskRunSummary }>(`/api/analytics/runs/${encodeURIComponent(runId)}${appendViewerTimezone("")}`)
}

export async function fetchTaskRunAttempts(runId: string): Promise<{ attempts: TaskRunAttempt[] }> {
  return apiFetch<{ attempts: TaskRunAttempt[] }>(`/api/analytics/runs/${encodeURIComponent(runId)}/attempts`)
}

export async function fetchTaskRunEvents(runId: string): Promise<{ events: TaskRunEvent[] }> {
  return apiFetch<{ events: TaskRunEvent[] }>(`/api/analytics/runs/${encodeURIComponent(runId)}/events`)
}

export async function fetchTaskRunPRs(runId: string): Promise<{ prs: TaskRunPR[] }> {
  return apiFetch<{ prs: TaskRunPR[] }>(`/api/analytics/runs/${encodeURIComponent(runId)}/prs`)
}

export async function fetchTaskRunOutputs(runId: string): Promise<{ outputs: TaskRunOutput[] }> {
  return apiFetch<{ outputs: TaskRunOutput[] }>(`/api/analytics/runs/${encodeURIComponent(runId)}/outputs`)
}

export async function fetchTaskRunFilterOptions(options?: AnalyticsRequestOptions): Promise<TaskRunFilterOptions> {
  return apiFetch<TaskRunFilterOptions>("/api/analytics/filter-options", options)
}
