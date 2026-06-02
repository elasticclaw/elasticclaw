// Centralized auth token storage.
// Tokens stay in sessionStorage so closing the browser clears the session. A
// BroadcastChannel copies tokens only between currently open tabs.

const GITHUB_TOKEN_KEY = "ec_github_token"
const HUB_TOKEN_KEY = "ec_hub_token"
const AUTH_CHANNEL = "elasticclaw-auth"

type AuthChannelMessage =
  | { type: "auth-token-request" }
  | { type: "auth-token-response"; githubToken: string | null; hubToken: string | null }
  | { type: "auth-token-updated"; githubToken: string | null; hubToken: string | null }
  | { type: "auth-token-cleared" }

let authChannel: BroadcastChannel | null = null

function storage(): Storage | null {
  if (typeof window === "undefined") return null
  return window.sessionStorage
}

function readStoredTokens(): { githubToken: string | null; hubToken: string | null } {
  const s = storage()
  if (!s) return { githubToken: null, hubToken: null }
  return {
    githubToken: s.getItem(GITHUB_TOKEN_KEY),
    hubToken: s.getItem(HUB_TOKEN_KEY),
  }
}

function storeTokens(githubToken: string | null, hubToken: string | null): void {
  const s = storage()
  if (!s) return
  if (githubToken) s.setItem(GITHUB_TOKEN_KEY, githubToken)
  if (hubToken) s.setItem(HUB_TOKEN_KEY, hubToken)
}

function removeStoredTokens(): void {
  const s = storage()
  if (!s) return
  s.removeItem(GITHUB_TOKEN_KEY)
  s.removeItem(HUB_TOKEN_KEY)
}

function postAuthMessage(message: AuthChannelMessage): void {
  getAuthChannel()?.postMessage(message)
}

function getAuthChannel(): BroadcastChannel | null {
  if (typeof window === "undefined" || typeof BroadcastChannel === "undefined") return null
  if (authChannel) return authChannel

  authChannel = new BroadcastChannel(AUTH_CHANNEL)
  authChannel.onmessage = (event: MessageEvent<AuthChannelMessage>) => {
    const message = event.data
    if (!message || typeof message.type !== "string") return
    if (message.type === "auth-token-request") {
      const { githubToken, hubToken } = readStoredTokens()
      if (githubToken || hubToken) {
        postAuthMessage({ type: "auth-token-response", githubToken, hubToken })
      }
      return
    }
    if (message.type === "auth-token-response" || message.type === "auth-token-updated") {
      storeTokens(message.githubToken, message.hubToken)
      return
    }
    if (message.type === "auth-token-cleared") {
      removeStoredTokens()
    }
  }
  return authChannel
}

if (typeof window !== "undefined") {
  getAuthChannel()
}

export function getAuthToken(): string | null {
  const { githubToken, hubToken } = readStoredTokens()
  return githubToken || hubToken || null
}

export function requestAuthToken(timeoutMs = 150): Promise<string | null> {
  const existing = getAuthToken()
  if (existing) return Promise.resolve(existing)

  const channel = getAuthChannel()
  if (!channel) return Promise.resolve(null)

  return new Promise((resolve) => {
    const timeout = window.setTimeout(() => {
      channel.removeEventListener("message", onMessage)
      resolve(getAuthToken())
    }, timeoutMs)
    const onMessage = (event: MessageEvent<AuthChannelMessage>) => {
      const message = event.data
      if (message?.type !== "auth-token-response" && message?.type !== "auth-token-updated") return
      storeTokens(message.githubToken, message.hubToken)
      window.clearTimeout(timeout)
      channel.removeEventListener("message", onMessage)
      resolve(getAuthToken())
    }
    channel.addEventListener("message", onMessage)
    channel.postMessage({ type: "auth-token-request" } satisfies AuthChannelMessage)
  })
}

export function getGitHubToken(): string | null {
  return readStoredTokens().githubToken
}

export function getHubToken(): string | null {
  return readStoredTokens().hubToken
}

export function setGitHubToken(token: string): void {
  const s = storage()
  if (!s) return
  s.setItem(GITHUB_TOKEN_KEY, token)
  const { hubToken } = readStoredTokens()
  postAuthMessage({ type: "auth-token-updated", githubToken: token, hubToken })
}

export function setHubToken(token: string): void {
  const s = storage()
  if (!s) return
  s.setItem(HUB_TOKEN_KEY, token)
  const { githubToken } = readStoredTokens()
  postAuthMessage({ type: "auth-token-updated", githubToken, hubToken: token })
}

export function clearAuthTokens(): void {
  removeStoredTokens()
  postAuthMessage({ type: "auth-token-cleared" })
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
