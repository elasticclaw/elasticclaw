"use client"

import { Suspense } from "react"
import { AppHeader } from "@/components/app-header"
import { AnalyticsCommandCenter } from "@/components/analytics-command-center"

export default function AnalyticsPage() {
  return (
    <div className="h-screen bg-background flex flex-col overflow-hidden">
      <AppHeader />
      <div className="flex-1 min-h-0">
        {/* AnalyticsCommandCenter reads its filters from the URL query via
            useSearchParams, which requires a Suspense boundary under static export. */}
        <Suspense fallback={null}>
          <AnalyticsCommandCenter />
        </Suspense>
      </div>
    </div>
  )
}
