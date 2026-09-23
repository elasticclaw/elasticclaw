"use client"

import { useSyncExternalStore } from "react"
import { fetchCurrentUser } from "@/lib/api"

// The enabled feature keys for the signed-in user, loaded once from
// /api/auth/me and shared by every useFeatureFlag caller on the page.
// null until the first load succeeds; a failed load is retried on the next
// subscribe instead of being cached as "no features".
let features: readonly string[] | null = null
let inflight: Promise<void> | null = null
let generation = 0
const listeners = new Set<() => void>()

function load() {
  if (inflight) return
  const started = generation
  inflight = fetchCurrentUser()
    .then((user) => {
      features = Array.isArray(user.features) ? user.features : []
      listeners.forEach((listener) => listener())
    })
    .catch(() => {})
    .finally(() => {
      inflight = null
      // A refresh landed while this request was in flight, so its answer may
      // predate the change: ask again.
      if (started !== generation) load()
    })
}

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
