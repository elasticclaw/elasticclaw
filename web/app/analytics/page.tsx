"use client"

import { Suspense } from "react"
import { NavRail } from "@/components/nav-rail"
import { AnalyticsCommandCenter } from "@/components/analytics-command-center"

export default function AnalyticsPage() {
  return (
    <div className="h-screen bg-background flex overflow-hidden">
      <NavRail />
      <div className="flex-1 min-w-0 min-h-0">
        {/* AnalyticsCommandCenter reads its filters from the URL query via
            useSearchParams, which requires a Suspense boundary under static export. */}
        <Suspense fallback={null}>
          <AnalyticsCommandCenter />
        </Suspense>
      </div>
    </div>
  )
}
