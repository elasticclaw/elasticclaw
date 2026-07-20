import { Suspense } from "react"
import { AnalyticsCommandCenter } from "@/components/analytics-command-center"

export default function AnalyticsPage() {
  return (
    <Suspense fallback={null}>
      <AnalyticsCommandCenter />
    </Suspense>
  )
}
