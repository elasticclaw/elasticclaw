import { getHubUrl } from "./hub-url"

export function artifactShareURL(slug: string, shareUrl?: string): string {
  const path = shareUrl || `/artifacts/${encodeURIComponent(slug)}`
  if (typeof window === "undefined") return path
  return new URL(path, getHubUrl() || window.location.origin).href
}

export function artifactLocation(path: string): { slug: string; version?: number } | null {
  // Slugs are lowercase ULIDs; title-based slugs from before stay valid.
  const match = /^\/artifacts\/([0-9a-hjkmnp-tv-z]{26}|[a-z0-9][a-z0-9-]{0,39}-[a-z2-7]{6})(?:\/v\/([1-9][0-9]*))?\/?$/.exec(path)
  if (!match) return null
  const version = match[2] ? Number(match[2]) : undefined
  if (version !== undefined && !Number.isSafeInteger(version)) return null
  return { slug: match[1], version }
}

export function artifactURLExpired(rawUrl: string, now = Date.now()): boolean {
  const expiry = Number(new URL(rawUrl, "https://hub.invalid").searchParams.get("exp"))
  return !expiry || expiry * 1000 <= now
}

export function artifactPageError(status?: number): string {
  return status === 404 ? "This artifact does not exist or was deleted." : "Unable to load this artifact. Try again."
}
