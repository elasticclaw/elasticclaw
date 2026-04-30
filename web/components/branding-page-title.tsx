"use client"

import { useEffect } from "react"
import { useBranding } from "@/hooks/use-branding"

const DEFAULT_TITLE = "ElasticClaw"

export function BrandingPageTitle() {
  const { appName } = useBranding()

  useEffect(() => {
    document.title = appName || DEFAULT_TITLE
  }, [appName])

  return null
}
