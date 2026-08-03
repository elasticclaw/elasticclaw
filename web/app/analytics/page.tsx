"use client"

import { Suspense } from "react"
import { PrimaryNavInline } from "@/components/primary-nav"
import { AnalyticsCommandCenter } from "@/components/analytics-command-center"

export default function AnalyticsPage() {
  return (
    <div className="h-screen bg-background flex flex-col overflow-hidden">
      {/* Slim top bar: AnalyticsCommandCenter renders its own "Analytics" heading,
          so the shell only provides the app-level primary navigation. */}
      <header className="border-b border-border px-4 py-2 flex items-center">
        <PrimaryNavInline />
      </header>
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
