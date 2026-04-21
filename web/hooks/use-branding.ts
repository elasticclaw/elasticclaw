"use client"

import { useState, useEffect } from "react"
import { getHubUrl } from "@/lib/hub-url"

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
    const token = typeof window !== "undefined" ? sessionStorage.getItem("ec_hub_token") || "" : ""
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
