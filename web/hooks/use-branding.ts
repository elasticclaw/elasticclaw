"use client"

import { useState, useEffect } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getHubToken } from "@/lib/auth-storage"

interface Branding {
  appName: string
  logoUrl: string
}

const DEFAULT: Branding = { appName: "ElasticClaw", logoUrl: "" }

let cached: Branding | null = null

export function useBranding(): Branding {
  const [branding, setBranding] = useState<Branding>(cached ?? DEFAULT)

  useEffect(() => {
    if (cached) return
    const hubUrl = getHubUrl()
    const token = typeof window !== "undefined" ? getHubToken() || "" : ""
    fetch(`${hubUrl}/api/hub-config`, { headers: { Authorization: `Bearer ${token}` } })
      .then(r => r.json())
      .then(d => {
        const b: Branding = {
          appName: d.appName || "ElasticClaw",
          logoUrl: d.logoUrl || "",
        }
        cached = b
        setBranding(b)
      })
      .catch(() => {})
  }, [])

  return branding
}
