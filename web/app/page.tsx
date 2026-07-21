"use client"

import { useState, useMemo, useCallback } from "react"
import { AppSidebar } from "@/components/app-sidebar"
import { ConversationView } from "@/components/conversation-view"
import { SetupScreen } from "@/components/setup-screen"
import { ManualTriggerModal } from "@/components/manual-trigger-modal"
import { useSidebarState } from "@/hooks/use-sidebar-state"
import type { Message } from "@/lib/types"
import { isConfigured, type Workflow } from "@/lib/api"

export default function Home() {
  const [configuredState, setConfiguredState] = useState<boolean | null>(() => {
    if (typeof window === "undefined") return null
    return isConfigured()
  })
  const [selectedWorkflow, setSelectedWorkflow] = useState<Workflow | null>(null)
  const sidebarState = useSidebarState()
  const { hub, claws, selectedClawId, setSelectedClawId } = sidebarState
  const { downtimeDependencies, messages, streamingBuffers, loading, hubError, reorderClaws } = hub

  // Build merged messages:
  // - no chunks yet → append a "thinking" placeholder
  // - chunks flowing → append the partial typewriter text
  // - done → nothing (final message already in messages)
  const mergedMessages = useMemo((): Record<string, Message[]> => {
    const result: Record<string, Message[]> = { ...messages }
    for (const [clawId, state] of Object.entries(streamingBuffers)) {
      const existing = result[clawId] || []
      if (!state.hadChunks) {
        // Thinking indicator
        result[clawId] = [...existing, {
          id: `thinking-${clawId}`,
          role: "claw" as const,
          content: "__THINKING__",
          timestamp: new Date(),
        }]
      } else {
        const msgs: Message[] = [...existing]
        if (state.text) {
          msgs.push({
            id: `streaming-${clawId}`,
            role: "claw" as const,
            content: state.text,
            timestamp: new Date(),
          })
        }
        result[clawId] = msgs
      }
    }
    return result
  }, [messages, streamingBuffers])

  const selectedClaw = useMemo(() => {
    return claws.find((c) => c.id === selectedClawId) ?? null
  }, [claws, selectedClawId])


  const handleSendMessage = useCallback(
    (content: string) => {
      if (!selectedClawId) return
      hub.send(selectedClawId, content)
    },
    [selectedClawId, hub]
  )

  const handleSendMessageToClaw = useCallback(
    (clawId: string, content: string) => {
      hub.send(clawId, content)
    },
    [hub]
  )

  const handleKill = useCallback(() => {
    if (!selectedClawId) return
    hub.killClaw(selectedClawId)
    setSelectedClawId(null)
    localStorage.removeItem('elasticclaw_selected_claw')
  }, [selectedClawId, hub])

  const handleKillClaw = useCallback(
    (clawId: string) => {
      hub.killClaw(clawId)
      if (selectedClawId === clawId) {
        setSelectedClawId(null)
        localStorage.removeItem('elasticclaw_selected_claw')
      }
    },
    [selectedClawId, hub]
  )

  // Show loading state until we know if configured
  if (configuredState === null) {
    return <div className="flex h-screen bg-background items-center justify-center" />
  }

  // Show setup screen if not configured
  if (!configuredState) {
    return <SetupScreen onConnected={() => setConfiguredState(true)} />
  }

  return (
    <div className="flex h-screen bg-background">
      <AppSidebar sidebarState={sidebarState} onSelectWorkflow={setSelectedWorkflow} />
      <div className="flex-1 flex flex-col min-w-0 overflow-hidden">
        <ConversationView
          claw={selectedClaw}
          allClaws={claws}
          downtimeDependencies={downtimeDependencies}
          messages={selectedClaw ? mergedMessages[selectedClaw.id] || [] : []}
          allMessages={mergedMessages}
          onSendMessage={handleSendMessage}
          onSendMessageToClaw={handleSendMessageToClaw}
          onKill={handleKill}
          onKillClaw={handleKillClaw}
          onSelectClaw={sidebarState.sidebarProps.onSelectClaw}
          onDeselectClaw={() => { setSelectedClawId(null); localStorage.removeItem('elasticclaw_selected_claw') }}
          onReorderClaws={reorderClaws}
          loading={loading}
          hubError={hubError}
        />
      </div>
      <ManualTriggerModal
        open={!!selectedWorkflow}
        onOpenChange={(open) => { if (!open) setSelectedWorkflow(null) }}
        workflow={selectedWorkflow}
      />
    </div>
  )
}
