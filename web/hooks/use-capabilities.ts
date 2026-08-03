"use client"

import { useCallback, useEffect, useState } from "react"
import { fetchAuthMe, hasCapability, type AuthMe } from "@/lib/api"

// Module-level promise cache so components mounting together (sidebar, /runs,
// /analytics) issue a single /api/auth/me request. On failure the cache is
// cleared so a later mount (or retry()) can refetch.
let mePromise: Promise<AuthMe> | null = null

function loadMe(): Promise<AuthMe> {
  if (!mePromise) {
    mePromise = fetchAuthMe().catch((err) => {
      mePromise = null
      throw err
    })
  }
  return mePromise
}

export interface UseCapabilitiesResult {
  me: AuthMe | null
  isAdmin: boolean
  canViewCosts: boolean
  loading: boolean
  // True when /api/auth/me could not be resolved (network error, hub restart,
  // proxy 5xx). Distinct from a successful response denying a capability:
  // consumers must render an error/retry state, never a hard "no access"
  // screen, when this is set.
  error: boolean
  retry: () => void
}

// useCapabilities fetches /api/auth/me once (deduplicated across mounts) and
// exposes capability flags. isAdmin and canViewCosts default to false while
// loading — never optimistically true.
export function useCapabilities(): UseCapabilitiesResult {
  const [me, setMe] = useState<AuthMe | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(false)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let cancelled = false
    loadMe().then(
      (data) => {
        if (cancelled) return
        setMe(data)
        setLoading(false)
      },
      () => {
        if (cancelled) return
        setMe(null)
        setError(true)
        setLoading(false)
      }
    )
    return () => {
      cancelled = true
    }
  }, [attempt])

  // loading/error are reset here (an event handler) rather than in the
  // effect body, which must not call setState synchronously.
  const retry = useCallback(() => {
    setLoading(true)
    setError(false)
    setAttempt((current) => current + 1)
  }, [])

  return {
    me,
    isAdmin: me?.is_admin === true,
    canViewCosts: hasCapability(me, "costs:read"),
    loading,
    error,
    retry,
  }
}
