"use client"

import { Suspense, useEffect } from "react"
import { useRouter, useSearchParams } from "next/navigation"

function AnalyticsRedirect() {
  const router = useRouter()
  const params = useSearchParams()

  useEffect(() => {
    const queryString = params.toString()
    router.replace(`/settings/workspace-analytics${queryString ? `?${queryString}` : ""}`)
  }, [params, router])

  return (
    <div className="flex h-full items-center justify-center p-6 text-sm text-muted-foreground">
      Redirecting to settings…
    </div>
  )
}

export default function AnalyticsPage() {
  return (
    <Suspense fallback={null}>
      <AnalyticsRedirect />
    </Suspense>
  )
}
