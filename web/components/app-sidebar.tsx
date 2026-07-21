"use client"

import { useCallback } from "react"
import { Sidebar } from "@/components/sidebar"
import type { SidebarState } from "@/hooks/use-sidebar-state"
import type { Workflow } from "@/lib/api"

interface AppSidebarProps {
  sidebarState: SidebarState
  onSelectClaw?: (id: string) => void
  onSelectWorkflow?: (workflow: Workflow | null) => void
}

export function AppSidebar({ sidebarState, onSelectClaw, onSelectWorkflow }: AppSidebarProps) {
  const handleSelectClaw = useCallback((id: string) => {
    sidebarState.sidebarProps.onSelectClaw(id)
    onSelectClaw?.(id)
  }, [sidebarState, onSelectClaw])

  return <Sidebar {...sidebarState.sidebarProps} onSelectClaw={handleSelectClaw} onSelectWorkflow={onSelectWorkflow} />
}
