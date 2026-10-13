"use client"

import { useEffect, useSyncExternalStore } from "react"
import { getHubUrl } from "@/lib/hub-url"

interface Branding {
  appName: string
  logoUrl: string
}

const DEFAULT: Branding = { appName: "ElasticClaw", logoUrl: "" }
const STORAGE_KEY = "elasticclaw_branding"

// Branding is public and rarely changes, so the last answer is kept across
// page loads: without it every load paints "ElasticClaw" and then swaps in the
// real name. The network is still asked once per page lifetime.
let fetched: Branding | null = null
let stored: Branding | null | undefined
let pending: Promise<void> | null = null
const listeners = new Set<() => void>()

function readStored(): Branding | null {
  if (stored !== undefined) return stored
  stored = null
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    const parsed = raw ? JSON.parse(raw) : null
    if (parsed && typeof parsed.appName === "string") {
      stored = { appName: parsed.appName, logoUrl: typeof parsed.logoUrl === "string" ? parsed.logoUrl : "" }
    }
  } catch { /* storage unavailable or corrupt — fetch decides */ }
  return stored
}

function loadBranding(): Promise<void> {
  if (fetched) return Promise.resolve()
  if (pending) return pending

  const hubUrl = getHubUrl()
  pending = fetch(`${hubUrl}/api/branding`)
    .then(r => r.ok ? r.json() : null)
    .then(d => {
      // Branding is non-critical; cache the fallback for this page lifetime
      // so shared layout consumers do not repeatedly retry a failed endpoint.
      fetched = {
        appName: d?.appName || DEFAULT.appName,
        logoUrl: d?.logoUrl || DEFAULT.logoUrl,
      }
      try { localStorage.setItem(STORAGE_KEY, JSON.stringify(fetched)) } catch { /* best effort */ }
    })
    .catch(() => {
      fetched = readStored() ?? DEFAULT
    })
    .finally(() => {
      pending = null
      for (const listener of listeners) listener()
    })
  return pending
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

function getSnapshot(): Branding | null {
  return fetched ?? readStored()
}

// null on the server: the static export cannot know the hub's name, so the
// prerendered page shows a placeholder and hydrates against the same thing.
function getServerSnapshot(): Branding | null {
  return null
}

/**
 * `loaded` is false until a name is known (from this page's fetch or the last
 * one). Headings render a placeholder until then; text that needs a name now
 * gets the default.
 */
export function useBranding(): Branding & { loaded: boolean } {
  const branding = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)

  useEffect(() => {
    void loadBranding()
  }, [])

  return branding ? { ...branding, loaded: true } : { ...DEFAULT, loaded: false }
}
