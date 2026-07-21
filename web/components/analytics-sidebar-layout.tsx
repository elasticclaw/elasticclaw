"use client"

import { useCallback } from "react"
import { useRouter } from "next/navigation"
import { AppSidebar } from "@/components/app-sidebar"
import { AnalyticsCommandCenter } from "@/components/analytics-command-center"
import { useSidebarState } from "@/hooks/use-sidebar-state"

export function AnalyticsSidebarLayout() {
  const router = useRouter()
  const sidebarState = useSidebarState()
  const goHomeWithClaw = useCallback(() => {
    router.push("/")
  }, [router])

  return (
    <div className="flex h-screen bg-background">
      <AppSidebar
        sidebarState={sidebarState}
        onSelectClaw={goHomeWithClaw}
        onSelectWorkflow={goHomeWithClaw}
      />
      <div className="flex-1 min-w-0 overflow-hidden">
        <AnalyticsCommandCenter />
      </div>
    </div>
  )
}
