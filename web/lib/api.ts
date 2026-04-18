import type { ApiClaw, ApiMessage, CreateClawRequest } from "./types"

// Token is resolved once and cached — fetched from /api/hub-config on first use
let _token: string | null = null
let _tokenPromise: Promise<string> | null = null

export function resolveToken(): Promise<string> {
  if (_token !== null) return Promise.resolve(_token)
  if (_tokenPromise) return _tokenPromise

  if (typeof window === "undefined") {
    // Server-side
    _token = process.env.HUB_TOKEN || process.env.NEXT_PUBLIC_HUB_TOKEN || ""
    return Promise.resolve(_token)
  }

  _tokenPromise = fetch("/api/hub-config")
    .then((r) => r.json())
    .then((d) => {
      _token = d.token || ""
      return _token!
    })
    .catch(() => {
      _token = process.env.NEXT_PUBLIC_HUB_TOKEN || ""
      return _token!
    })

  return _tokenPromise
}

// Sync getter — returns cached token or empty string; call resolveToken() first
function getTokenSync(): string {
  return _token || process.env.NEXT_PUBLIC_HUB_TOKEN || ""
}

// Pre-fetch token on module load (client-side)
if (typeof window !== "undefined") {
  resolveToken()
}

async function apiFetch<T>(path: string, options?: RequestInit): Promise<T> {
  const token = await resolveToken()
  const url = `/hub${path}`
  const res = await fetch(url, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
      "ngrok-skip-browser-warning": "true", // skip ngrok interstitial in dev
      ...options?.headers,
    },
  })
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await res.text()}`)
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
  const wsBase = window.location.origin.replace(/^https:/, "wss:").replace(/^http:/, "ws:")
  return `${wsBase}/hub/api/ws?token=${encodeURIComponent(token)}`
}

export function getTerminalWsUrl(clawId: string): string {
  const token = getTokenSync()
  const wsBase = window.location.origin.replace(/^https:/, "wss:").replace(/^http:/, "ws:")
  return `${wsBase}/hub/api/terminal/${clawId}?token=${encodeURIComponent(token)}`
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
}

export function getConfig() {
  return {
    hubUrl: "",
    token: getTokenSync(),
  }
}

export async function patchClaw(clawId: string, patch: { tags?: string[]; color?: string }): Promise<void> {
  await apiFetch(`/api/claws/${clawId}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  })
}
