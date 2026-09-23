"use client"

import { useSyncExternalStore } from "react"
import { fetchCurrentUser, onAuthCleared } from "@/lib/api"
import { getAuthToken } from "@/lib/auth-storage"

// The enabled feature keys for the signed-in user, loaded once from
// /api/auth/me and shared by every useFeatureFlag caller on the page.
// null until the first load succeeds; a failed load is retried after a delay
// while anything is subscribed, instead of being cached as "no features".
let features: readonly string[] | null = null
let inflight: Promise<void> | null = null
let retryTimer: ReturnType<typeof setTimeout> | null = null
const RETRY_DELAY_MS = 5000
let generation = 0
const listeners = new Set<() => void>()

// Automatic reloads (retry, post-refresh, post-sign-in) only make sense with a
// session: without one /api/auth/me answers 401, which clears the session
// again and would re-trigger the reload in a tight loop.
function hasSession() {
  return getAuthToken() !== null
}

function load() {
  if (inflight) return
  const started = generation
  inflight = fetchCurrentUser()
    .then((user) => {
      // A newer request is coming (refresh or session change); this answer
      // may be stale or belong to the previous user.
      if (started !== generation) return
      features = Array.isArray(user.features) ? user.features : []
      listeners.forEach((listener) => listener())
    })
    .catch(() => {
      if (listeners.size > 0 && retryTimer === null && hasSession()) {
        retryTimer = setTimeout(() => {
          retryTimer = null
          if (listeners.size > 0) load()
        }, RETRY_DELAY_MS)
      }
    })
    .finally(() => {
      inflight = null
      // A refresh landed while this request was in flight, so its answer may
      // predate the change: ask again.
      if (started !== generation && hasSession()) load()
    })
}

// The cache belongs to one session: drop it when the session changes so the
// next user never sees the previous user's features.
onAuthCleared(() => {
  generation++
  features = null
  if (retryTimer !== null) {
    clearTimeout(retryTimer)
    retryTimer = null
  }
  listeners.forEach((listener) => listener())
  // Sign-in sets the new token synchronously after clearing the old one.
  queueMicrotask(() => {
    if (listeners.size > 0 && hasSession()) load()
  })
})

function subscribe(listener: () => void) {
  listeners.add(listener)
  if (features === null) load()
  return () => {
    listeners.delete(listener)
  }
}

function getSnapshot() {
  return features
}

function getServerSnapshot() {
  return null
}

/**
 * Re-reads the current user's features. Keeps serving the previous answer
 * until the new one arrives, so gated UI does not flicker off.
 */
export function refreshFeatureFlags() {
  generation++
  load()
}

/**
 * Whether the feature flag `key` is enabled for the signed-in user: the flag
 * is "on", or it is in "beta" and the user is a beta tester. False while
 * loading and for unknown keys.
 */
export function useFeatureFlag(key: string): boolean {
  const enabled = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
  return enabled?.includes(key) ?? false
}
