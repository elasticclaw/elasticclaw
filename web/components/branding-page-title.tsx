"use client"

import { useEffect } from "react"
import { useBranding } from "@/hooks/use-branding"

export function BrandingPageTitle() {
  const { appName, loaded } = useBranding()

  // Until the name is known the static title stands; writing the default here
  // would only be overwritten a moment later.
  useEffect(() => {
    if (loaded) document.title = appName
  }, [appName, loaded])

  return null
}
