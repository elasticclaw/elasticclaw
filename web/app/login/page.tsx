"use client"

import { useState, useEffect, Suspense } from "react"
import { useRouter, useSearchParams } from "next/navigation"
import { getHubUrl } from "@/lib/hub-url"
import { clearConfig } from "@/lib/api"
import { setGitHubToken, setHubToken, getOAuthState, setOAuthState, removeOAuthState, getOAuthNext, setOAuthNext, removeOAuthNext } from "@/lib/auth-storage"
import { useBranding } from "@/hooks/use-branding"

interface AuthConfig {
  github_oauth_enabled: boolean
  password_auth_enabled: boolean
}

function safeNextPath(next: string | null): string | null {
  if (!next) return null
  if (!next.startsWith("/") || next.startsWith("//")) return null
  return next
}

function randomState(): string {
  const buf = new Uint8Array(16)
  crypto.getRandomValues(buf)
  return btoa(String.fromCharCode(...buf)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")
}

function LoginForm() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const { appName } = useBranding()
  const nextParam = safeNextPath(searchParams.get("next"))
  const next = nextParam || "/"

  const [password, setPassword] = useState("")
  const [error, setError] = useState("")
  const [loading, setLoading] = useState(false)
  const [authConfig, setAuthConfig] = useState<AuthConfig | null>(null)

  useEffect(() => {
    const hubUrl = getHubUrl()
    const configUrl = hubUrl ? `${hubUrl}/api/auth/config` : "/api/auth/config"
    fetch(configUrl)
      .then((r) => r.ok ? r.json() : null)
      .then((data) => { if (data) setAuthConfig(data) })
      .catch(() => {})
  }, [])

  // Handle GitHub OAuth callback — GitHub redirects back here with ?code=&state=
  useEffect(() => {
    const code = searchParams.get("code")
    const state = searchParams.get("state")
    if (!code || !state) return

    // Validate state to prevent CSRF
    const storedState = getOAuthState()
    if (!storedState || state !== storedState) {
      setError("OAuth state mismatch — please try again")
      return
    }
    removeOAuthState()
    const storedNext = safeNextPath(getOAuthNext())
    removeOAuthNext()
    const callbackNext = nextParam || storedNext || "/"

    const redirectUri = window.location.origin + "/login"
    const hubUrl = getHubUrl()
    const exchangeUrl = hubUrl ? `${hubUrl}/api/auth/github/exchange` : "/api/auth/github/exchange"

    let cancelled = false
    fetch(exchangeUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code, redirect_uri: redirectUri }),
    })
      .then(async (res) => {
        if (!res.ok) throw new Error(await res.text())
        return res.json()
      })
      .then((data) => {
        if (cancelled) return
        if (data.github_token) {
          clearConfig()
          setGitHubToken(data.github_token)
          router.replace(callbackNext)
        } else {
          throw new Error("missing token")
        }
      })
      .catch((err) => {
        if (!cancelled) setError("GitHub sign-in failed: " + (err?.message || "unknown error"))
      })

    return () => { cancelled = true }
  }, [searchParams, nextParam, router])

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setLoading(true)
    setError("")

    try {
      const hubUrl = getHubUrl()
      const loginUrl = hubUrl ? `${hubUrl}/api/auth/login` : "/api/auth/login"
      const res = await fetch(loginUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      })

      if (res.ok) {
        const data = await res.json()
        if (data.hubToken) {
          clearConfig()
          setHubToken(data.hubToken)
        }
        try {
          const { getHubUrl } = await import("@/lib/hub-url")
          const hubUrl = getHubUrl()
          const statusRes = await fetch(`${hubUrl}/api/settings/status`, {
            headers: { Authorization: `Bearer ${data.hubToken}` },
          })
          if (statusRes.ok) {
            const status = await statusRes.json()
            if (!status.hasProvider || !status.hasLLMKey) {
              router.push("/settings")
              return
            }
          }
        } catch { /* ignore */ }
        router.push(next)
        router.refresh()
      } else {
        setError("Invalid password")
      }
    } catch {
      setError("Something went wrong")
    } finally {
      setLoading(false)
    }
  }

  function handleGitHubSignIn() {
    const hubUrl = getHubUrl()
    // Ask the hub for the client_id (already public in /api/auth/config)
    // Build the GitHub authorize URL entirely in the browser.
    const state = randomState()
    setOAuthState(state)
    if (nextParam) setOAuthNext(nextParam)

    const redirectUri = window.location.origin + "/login"

    // We need the GitHub client_id — fetch it from the hub's auth config
    // (it's already exposed there as a public field via github_oauth_enabled).
    // We store it in authConfig but need the actual client_id — fetch separately.
    const configUrl = hubUrl ? `${hubUrl}/api/auth/github/client-id` : "/api/auth/github/client-id"
    fetch(configUrl)
      .then((r) => r.ok ? r.json() : Promise.reject("config unavailable"))
      .then(({ client_id }: { client_id: string }) => {
        const params = new URLSearchParams({
          client_id,
          redirect_uri: redirectUri,
          scope: "read:user read:org",
          state,
        })
        window.location.href = "https://github.com/login/oauth/authorize?" + params.toString()
      })
      .catch(() => setError("Could not load GitHub OAuth config"))
  }

  const showGitHub = authConfig?.github_oauth_enabled
  const showPassword = !authConfig || authConfig.password_auth_enabled
  // If we have a code param we're mid-callback — show a loading state
  const isCallback = !!searchParams.get("code")

  return (
    <div className="flex h-screen bg-background items-center justify-center">
      <div className="w-full max-w-sm space-y-6 p-8 border border-border rounded-xl bg-card">
        <div className="space-y-1 text-center">
          <h1 className="text-2xl font-semibold tracking-tight">{appName}</h1>
          <p className="text-sm text-muted-foreground">
            {isCallback ? "Completing sign-in…" : "Sign in to continue"}
          </p>
        </div>

        {isCallback && !error && (
          <p className="text-center text-sm text-muted-foreground animate-pulse">Signing you in with GitHub…</p>
        )}

        {error && <p className="text-sm text-red-500 text-center">{error}</p>}

        {!isCallback && (
          <>
            {showGitHub && (
              <button
                type="button"
                onClick={handleGitHubSignIn}
                className="w-full flex items-center justify-center gap-2 rounded-md border border-border bg-background px-3 py-2 text-sm font-medium hover:bg-accent hover:text-accent-foreground"
              >
                <svg viewBox="0 0 16 16" className="h-4 w-4 fill-current" aria-hidden="true">
                  <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8z" />
                </svg>
                Sign in with GitHub
              </button>
            )}

            {showGitHub && showPassword && (
              <div className="relative">
                <div className="absolute inset-0 flex items-center">
                  <span className="w-full border-t border-border" />
                </div>
                <div className="relative flex justify-center text-xs uppercase">
                  <span className="bg-card px-2 text-muted-foreground">or</span>
                </div>
              </div>
            )}

            {showPassword && (
              <form onSubmit={handleSubmit} className="space-y-4">
                <input
                  type="password"
                  placeholder="Access token"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoFocus={!showGitHub}
                  className="w-full rounded-md border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
                />
                <button
                  type="submit"
                  disabled={loading || !password}
                  className="w-full rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
                >
                  {loading ? "Signing in…" : "Sign in"}
                </button>
              </form>
            )}
          </>
        )}
      </div>
    </div>
  )
}

function LoginFallback() {
  const { appName } = useBranding()

  return (
    <div className="flex h-screen bg-background items-center justify-center">
      <div className="w-full max-w-sm space-y-6 p-8 border border-border rounded-xl bg-card">
        <div className="space-y-1 text-center">
          <h1 className="text-2xl font-semibold tracking-tight">{appName}</h1>
          <p className="text-sm text-muted-foreground">Loading…</p>
        </div>
      </div>
    </div>
  )
}

export default function LoginPage() {
  return (
    <Suspense fallback={<LoginFallback />}>
      <LoginForm />
    </Suspense>
  )
}
