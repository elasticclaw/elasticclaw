"use client"

import { Suspense } from "react"
import { AnalyticsSidebarLayout } from "@/components/analytics-sidebar-layout"

export default function AnalyticsPage() {
  return (
    <Suspense fallback={null}>
      <AnalyticsSidebarLayout />
    </Suspense>
  )
}
