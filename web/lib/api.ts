import type { ApiClaw, ApiMessage, CreateClawRequest } from "./types"
import { getHubUrl, setHubUrl } from "./hub-url"

// Token is resolved once and cached.
// Priority: sessionStorage (set on login) > /api/hub-config (proxy to hub)
let _token: string | null = null
let _tokenPromise: Promise<string> | null = null

export function resolveToken(): Promise<string> {
  if (_token) return Promise.resolve(_token)
  if (_tokenPromise) return _tokenPromise

  if (typeof window === "undefined") {
    // Server-side: no token (hub is the authority)
    _token = ""
    return Promise.resolve(_token)
  }

  // Check sessionStorage first (set by login page)
  // Prefer GitHub OAuth token over hub token when both exist
  const githubToken = sessionStorage.getItem("ec_github_token")
  if (githubToken) {
    _token = githubToken
    return Promise.resolve(_token)
  }
  const stored = sessionStorage.getItem("ec_hub_token")
  if (stored) {
    _token = stored
    return Promise.resolve(_token)
  }

  // Fall back to hub-config proxy (supports dev mode with Next.js)
  const hubUrl = getHubUrl()
  const hubConfigUrl = hubUrl ? `${hubUrl}/api/hub-config` : "/api/hub-config"

  _tokenPromise = fetch(hubConfigUrl, {
    headers: { Authorization: `Bearer ${sessionStorage.getItem("ec_hub_token") || ""}` }
  })
    .then((r) => r.json())
    .then((d) => {
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
    return sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
  }
  return ""
}

// Pre-fetch token on module load (client-side)
if (typeof window !== "undefined") {
  resolveToken()
}

async function apiFetch<T>(path: string, options?: RequestInit): Promise<T> {
  const token = await resolveToken()
  const hubBase = getHubUrl()
  const url = hubBase ? `${hubBase}${path}` : `/hub${path}`
  const res = await fetch(url, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
      ...options?.headers,
    },
  })
  if (res.status === 401) {
    // Token expired or invalid — clear it and redirect to login
    clearConfig()
    if (typeof window !== "undefined" && !window.location.pathname.startsWith("/login")) {
      window.location.href = "/login"
      // Return a never-resolving promise to prevent the error from propagating
      // and triggering the "Cannot reach hub" error screen before navigation completes
      return new Promise(() => {})
    }
    throw new Error("session expired")
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
    throw new Error(message)
  }
  if (res.status === 204) return undefined as T
  return res.json()
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
  const base = hubBase ? `${hubBase}/api/files/view/${clawId}` : `/hub/api/files/view/${clawId}`
  const qs = new URLSearchParams({ path, token }).toString()
  return `${base}?${qs}`
}

export async function uploadFiles(clawId: string, files: File[]): Promise<UploadedAttachment[]> {
  const token = await resolveToken()
  const hubBase = getHubUrl()
  const url = hubBase ? `${hubBase}/api/files/${clawId}` : `/hub/api/files/${clawId}`
  const form = new FormData()
  for (const f of files) form.append("files", f, f.name)
  const res = await fetch(url, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
    body: form,
  })
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
  if (typeof window !== "undefined") {
    sessionStorage.removeItem("ec_hub_token")
    sessionStorage.removeItem("ec_github_token")
  }
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

export async function fetchClawAutoSettings(clawId: string): Promise<{ autoFixCI: boolean; autoFixBugbot: boolean }> {
  return apiFetch(`/api/claws/${clawId}/settings`)
}

export async function patchClawAutoSettings(clawId: string, patch: { autoFixCI?: boolean; autoFixBugbot?: boolean }): Promise<void> {
  await apiFetch(`/api/claws/${clawId}/settings`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  })
}

// Factory types for manual trigger
export interface FactoryInput {
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

// Factory matches FactoryPushView from GET /api/factories.
// Field names match the JSON tags from the backend (snake_case / lowercase).
export interface Factory {
  name: string
  integration: string
  workspace: string
  trigger_status: string
  done_status?: string
  template: string
  labels?: string[]
  assigned_to?: string
  enabled?: boolean
  has_webhook_secret: boolean
  webhook_secret_ref?: string
  pipeline_yaml?: string
  enable_manual_trigger?: boolean
  secret_refs?: Record<string, string>
  inputs?: FactoryInput[]
}

export async function fetchFactories(): Promise<Factory[]> {
  return apiFetch<Factory[]>("/api/factories")
}

export async function triggerFactory(name: string, inputs?: Record<string, unknown>): Promise<{ claw_id: string; status: string }> {
  return apiFetch<{ claw_id: string; status: string }>(`/api/factories/${encodeURIComponent(name)}/trigger`, {
    method: "POST",
    body: JSON.stringify({ inputs: inputs || {} }),
  })
}
