"use client"

import { useState, useMemo, useEffect, useCallback, useRef } from "react"
import { Sidebar } from "@/components/sidebar"
import { ConversationView } from "@/components/conversation-view"
import { SetupScreen } from "@/components/setup-screen"
import { ManualTriggerModal } from "@/components/manual-trigger-modal"
import { useHub } from "@/hooks/use-hub"
import type { Message } from "@/lib/types"
import { isConfigured, type Workflow } from "@/lib/api"
import { requestAuthToken } from "@/lib/auth-storage"

export default function Home() {
  const [selectedClawId, setSelectedClawId] = useState<string | null>(() => {
    if (typeof window === 'undefined') return null
    return localStorage.getItem('elasticclaw_selected_claw') ?? null
  })
  const [searchQuery, setSearchQuery] = useState("")
  const [activeTagFilters, setActiveTagFilters] = useState<string[]>([])
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  const [configuredState, setConfiguredState] = useState<boolean | null>(null)
  const [isAdmin, setIsAdmin] = useState(false)
  const [selectedWorkflow, setSelectedWorkflow] = useState<Workflow | null>(null)

  // Check configured on mount (needs browser for localStorage)
  useEffect(() => {
    setConfiguredState(isConfigured())
  }, [])

  // Fetch admin status
  useEffect(() => {
    let cancelled = false
    async function loadAdminStatus() {
      const token = await requestAuthToken()
      if (cancelled) return
      if (!token) {
        setIsAdmin(false)
        return
      }
      const { getHubUrl } = await import("@/lib/hub-url")
      if (cancelled) return
      const hubUrl = getHubUrl()
      const url = hubUrl ? `${hubUrl}/api/auth/me` : "/api/auth/me"
      fetch(url, { headers: { Authorization: `Bearer ${token}` } })
        .then(r => r.ok ? r.json() : null)
        .then(data => { if (!cancelled) setIsAdmin(data?.is_admin === true) })
        .catch(() => { if (!cancelled) setIsAdmin(false) })
    }
    loadAdminStatus()
    return () => { cancelled = true }
  }, [])

  const hub = useHub(selectedClawId)

  const { claws: rawClaws, messages, streamingBuffers, loading, hubError, reorderClaws } = hub

  // Derive isStreaming from the live typewriter buffers so it stays true
  // while the typewriter is still draining (even after final WS message arrives)
  const claws = useMemo(() =>
    rawClaws.map((c) => ({
      ...c,
      // Show spinner while buffer exists (typewriter draining) OR while hub says streaming.
      // But if hub already set isStreaming=false (onDrain fired), respect that even if
      // the display buffer hasn't flushed yet.
      isStreaming: c.isStreaming || Boolean(streamingBuffers[c.id]?.hadChunks && streamingBuffers[c.id]?.text),
    })),
    [rawClaws, streamingBuffers]
  )

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

  // Collect all unique tags from all claws
  const allTags = useMemo(() => {
    const tagSet = new Set<string>()
    claws.forEach((claw) => claw.tags.forEach((t) => tagSet.add(t)))
    return Array.from(tagSet).sort()
  }, [claws])

  const selectedClaw = useMemo(() => {
    return claws.find((c) => c.id === selectedClawId) ?? null
  }, [claws, selectedClawId])

  const pinnedClaws = useMemo(() => claws.filter((c) => c.pinned), [claws])
  const unpinnedClaws = useMemo(() => claws.filter((c) => !c.pinned), [claws])

  const filteredClaws = useMemo(() => {
    let result = searchQuery.trim() ? claws : unpinnedClaws

    if (searchQuery.trim()) {
      const query = searchQuery.toLowerCase()
      result = result.filter(
        (c) =>
          c.name.toLowerCase().includes(query) ||
          c.tags.some((tag) => tag.toLowerCase().includes(query))
      )
    }

    if (activeTagFilters.length > 0) {
      result = result.filter((c) =>
        activeTagFilters.every((tag) => c.tags.includes(tag))
      )
    }

    return result
  }, [claws, unpinnedClaws, searchQuery, activeTagFilters])

  const filteredPinnedClaws = useMemo(() => {
    if (activeTagFilters.length === 0) return pinnedClaws
    return pinnedClaws.filter((c) =>
      activeTagFilters.every((tag) => c.tags.includes(tag))
    )
  }, [pinnedClaws, activeTagFilters])

    // Eagerly load messages for all claws once the claw list is first available.
  // Covers: initial load, refresh, navigating back from /settings.
  // Board cards are passive — they never trigger loadMessages themselves.
  const boardLoadedRef = useRef(false)
  useEffect(() => {
    if (boardLoadedRef.current || claws.length === 0) return
    boardLoadedRef.current = true
    for (const c of claws) {
      hub.loadMessages(c.id)
    }
  }, [claws]) // re-runs when claws first populate

  // Mark messages as read when selecting a claw + lazy load history
  const handleSelectClaw = useCallback(
    (id: string) => {
      setSelectedClawId(id)
      localStorage.setItem('elasticclaw_selected_claw', id)
      hub.setUnreadCount(id, 0)
      hub.loadMessages(id)
    },
    [hub]
  )

  // When in board view (no claw selected), all cards are visible — clear unread
  // for any claw that has messages showing on screen.
  useEffect(() => {
    if (selectedClawId) return
    const withUnread = claws.filter((c) => c.unreadCount > 0)
    if (withUnread.length === 0) return
    for (const c of withUnread) {
      hub.setUnreadCount(c.id, 0)
    }
  }, [selectedClawId, claws, hub])

  const handleTogglePin = useCallback(
    (id: string) => {
      const claw = claws.find((c) => c.id === id)
      if (claw) hub.setPinned(id, !claw.pinned)
    },
    [claws, hub]
  )

  const handleAddTagFilter = useCallback((tag: string) => {
    setActiveTagFilters((prev) => prev.includes(tag) ? prev : [...prev, tag])
  }, [])

  const handleRemoveTagFilter = useCallback((tag: string) => {
    setActiveTagFilters((prev) => prev.filter((t) => t !== tag))
  }, [])

  const handleClearTagFilters = useCallback(() => {
    setActiveTagFilters([])
  }, [])

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
      <Sidebar
        claws={filteredClaws}
        pinnedClaws={filteredPinnedClaws}
        allClawIds={claws.map((c) => c.id)}
        selectedClawId={selectedClawId}
        onSelectClaw={handleSelectClaw}
        onTogglePin={handleTogglePin}
        searchQuery={searchQuery}
        onSearchChange={setSearchQuery}
        allTags={allTags}
        activeTagFilters={activeTagFilters}
        onAddTagFilter={handleAddTagFilter}
        onRemoveTagFilter={handleRemoveTagFilter}
        onClearTagFilters={handleClearTagFilters}
        isCollapsed={sidebarCollapsed}
        onToggleCollapse={() => setSidebarCollapsed(!sidebarCollapsed)}
        onReorderClaws={reorderClaws}
        isAdmin={isAdmin}
        onSelectWorkflow={setSelectedWorkflow}
      />
      <div className="flex-1 flex flex-col min-w-0 overflow-hidden">
        <ConversationView
          claw={selectedClaw}
          allClaws={claws}
          messages={selectedClaw ? mergedMessages[selectedClaw.id] || [] : []}
          allMessages={mergedMessages}
          onSendMessage={handleSendMessage}
          onSendMessageToClaw={handleSendMessageToClaw}
          onKill={handleKill}
          onKillClaw={handleKillClaw}
          onSelectClaw={handleSelectClaw}
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
