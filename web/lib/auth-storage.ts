// Centralized auth token storage — uses localStorage so sessions persist across tabs.
// Previously sessionStorage was used, which is scoped to a single tab.

const GITHUB_TOKEN_KEY = "ec_github_token"
const HUB_TOKEN_KEY = "ec_hub_token"

export function getAuthToken(): string | null {
  if (typeof window === "undefined") return null
  return localStorage.getItem(GITHUB_TOKEN_KEY) || localStorage.getItem(HUB_TOKEN_KEY) || null
}

export function getGitHubToken(): string | null {
  if (typeof window === "undefined") return null
  return localStorage.getItem(GITHUB_TOKEN_KEY)
}

export function getHubToken(): string | null {
  if (typeof window === "undefined") return null
  return localStorage.getItem(HUB_TOKEN_KEY)
}

export function setGitHubToken(token: string): void {
  if (typeof window === "undefined") return
  localStorage.setItem(GITHUB_TOKEN_KEY, token)
}

export function setHubToken(token: string): void {
  if (typeof window === "undefined") return
  localStorage.setItem(HUB_TOKEN_KEY, token)
}

export function clearAuthTokens(): void {
  if (typeof window === "undefined") return
  localStorage.removeItem(GITHUB_TOKEN_KEY)
  localStorage.removeItem(HUB_TOKEN_KEY)
}

// OAuth flow state (still sessionStorage — these are per-tab OAuth CSRF state)
const OAUTH_STATE_KEY = "oauth_state"
const OAUTH_NEXT_KEY = "oauth_next"

export function getOAuthState(): string | null {
  if (typeof window === "undefined") return null
  return sessionStorage.getItem(OAUTH_STATE_KEY)
}

export function setOAuthState(state: string): void {
  if (typeof window === "undefined") return
  sessionStorage.setItem(OAUTH_STATE_KEY, state)
}

export function removeOAuthState(): void {
  if (typeof window === "undefined") return
  sessionStorage.removeItem(OAUTH_STATE_KEY)
}

export function getOAuthNext(): string | null {
  if (typeof window === "undefined") return null
  return sessionStorage.getItem(OAUTH_NEXT_KEY)
}

export function setOAuthNext(next: string): void {
  if (typeof window === "undefined") return
  sessionStorage.setItem(OAUTH_NEXT_KEY, next)
}

export function removeOAuthNext(): void {
  if (typeof window === "undefined") return
  sessionStorage.removeItem(OAUTH_NEXT_KEY)
}
