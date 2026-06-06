"use client"

import { useState, useEffect } from "react"
import { getHubUrl } from "@/lib/hub-url"

interface Branding {
  appName: string
  logoUrl: string
}

const DEFAULT: Branding = { appName: "ElasticClaw", logoUrl: "" }

let cached: Branding | null = null
let pending: Promise<Branding> | null = null

function loadBranding(): Promise<Branding> {
  if (cached) return Promise.resolve(cached)
  if (pending) return pending

  const hubUrl = getHubUrl()
  pending = fetch(`${hubUrl}/api/branding`)
    .then(r => r.ok ? r.json() : null)
    .then(d => {
      // Branding is non-critical; cache the fallback for this page lifetime
      // so shared layout consumers do not repeatedly retry a failed endpoint.
      const branding: Branding = {
        appName: d?.appName || DEFAULT.appName,
        logoUrl: d?.logoUrl || DEFAULT.logoUrl,
      }
      cached = branding
      return branding
    })
    .catch(() => {
      cached = DEFAULT
      return DEFAULT
    })
    .finally(() => {
      pending = null
    })
  return pending
}

export function useBranding(): Branding {
  const [branding, setBranding] = useState<Branding>(cached ?? DEFAULT)

  useEffect(() => {
    if (cached) return
    loadBranding().then(setBranding)
  }, [])

  return branding
}
