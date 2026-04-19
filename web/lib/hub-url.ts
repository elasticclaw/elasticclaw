// Hub URL resolution — safe to expose as NEXT_PUBLIC_ since it's just a server address.
// The hub token (secret) is never exposed this way.

let _hubUrl: string | null = null

export function getHubUrl(): string {
  if (_hubUrl) return _hubUrl
  if (typeof window === "undefined") return ""
  // Injected at build time via NEXT_PUBLIC_HUB_URL, or falls back to same origin
  const fromEnv = process.env.NEXT_PUBLIC_HUB_URL
  _hubUrl = fromEnv || window.location.origin.replace(/:\d+$/, ":8080") || ""
  return _hubUrl
}

export function setHubUrl(url: string) {
  _hubUrl = url
}
