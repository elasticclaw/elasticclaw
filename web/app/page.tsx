"use client"

import { useState, useMemo, useEffect, useCallback, useRef, useSyncExternalStore } from "react"
import { AppHeader } from "@/components/app-header"
import { Sidebar } from "@/components/sidebar"
import { ConversationView } from "@/components/conversation-view"
import { SetupScreen } from "@/components/setup-screen"
import { ManualTriggerModal } from "@/components/manual-trigger-modal"
import { useHub } from "@/hooks/use-hub"
import { isConfigured, type Workflow } from "@/lib/api"

// Stable no-op subscription for useSyncExternalStore: the configured flag
// only changes via SetupScreen (handled by configuredOverride below).
const subscribeNever = () => () => {}

export default function Home() {
  const [selectedClawId, setSelectedClawId] = useState<string | null>(() => {
    if (typeof window === 'undefined') return null
    return localStorage.getItem('elasticclaw_selected_claw') ?? null
  })
  const [searchQuery, setSearchQuery] = useState("")
  const [activeTagFilters, setActiveTagFilters] = useState<string[]>([])
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  // isConfigured() reads localStorage, so it must not run during SSR or the
  // hydration render (the server rendered the placeholder branch and a lazy
  // initializer here caused a hydration mismatch). useSyncExternalStore gives
  // a hydration-safe read: the server snapshot is null and the real value
  // resolves right after hydration. setConfiguredOverride lets SetupScreen
  // flip to configured without a reload.
  const storedConfigured = useSyncExternalStore<boolean | null>(
    subscribeNever,
    isConfigured,
    () => null
  )
  const [configuredOverride, setConfiguredOverride] = useState<boolean | null>(null)
  const configuredState = configuredOverride ?? storedConfigured
  const [selectedWorkflow, setSelectedWorkflow] = useState<Workflow | null>(null)

  const hub = useHub(selectedClawId)

  const {
    claws: rawClaws,
    downtimeDependencies,
    messages,
    streamingBuffers,
    loading,
    hubError,
    reorderClaws,
    loadMessages,
    setUnreadCount,
    setPinned,
    send,
    killClaw,
  } = hub

  const claws = rawClaws

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
      loadMessages(c.id)
    }
  }, [claws, loadMessages]) // re-runs when claws first populate

  // Mark messages as read when selecting a claw + lazy load history
  const handleSelectClaw = useCallback(
    (id: string) => {
      setSelectedClawId(id)
      localStorage.setItem('elasticclaw_selected_claw', id)
      setUnreadCount(id, 0)
      loadMessages(id)
    },
    [loadMessages, setUnreadCount]
  )

  // When in board view (no claw selected), all cards are visible — clear unread
  // for any claw that has messages showing on screen.
  useEffect(() => {
    if (selectedClawId) return
    const withUnread = claws.filter((c) => c.unreadCount > 0)
    if (withUnread.length === 0) return
    for (const c of withUnread) {
      setUnreadCount(c.id, 0)
    }
  }, [selectedClawId, claws, setUnreadCount])

  const handleTogglePin = useCallback(
    (id: string) => {
      const claw = claws.find((c) => c.id === id)
      if (claw) setPinned(id, !claw.pinned)
    },
    [claws, setPinned]
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
      send(selectedClawId, content)
    },
    [selectedClawId, send]
  )

  const handleSendMessageToClaw = useCallback(
    (clawId: string, content: string) => {
      send(clawId, content)
    },
    [send]
  )

  const handleKill = useCallback(() => {
    if (!selectedClawId) return
    killClaw(selectedClawId)
    setSelectedClawId(null)
    localStorage.removeItem('elasticclaw_selected_claw')
  }, [selectedClawId, killClaw])

  const handleKillClaw = useCallback(
    (clawId: string) => {
      killClaw(clawId)
      if (selectedClawId === clawId) {
        setSelectedClawId(null)
        localStorage.removeItem('elasticclaw_selected_claw')
      }
    },
    [selectedClawId, killClaw]
  )

  // Show loading state until we know if configured
  if (configuredState === null) {
    // Mirror the real render's shell (same outer div and AppHeader) so the
    // server and client markup match and the header does not pop in.
    return (
      <div className="flex h-screen flex-col bg-background">
        <AppHeader />
        <div className="flex flex-1 min-h-0" />
      </div>
    )
  }

  // Show setup screen if not configured
  if (!configuredState) {
    return <SetupScreen onConnected={() => setConfiguredOverride(true)} />
  }

  return (
    <div className="flex h-screen flex-col bg-background">
      <AppHeader />
      <div className="flex flex-1 min-h-0">
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
          onSelectWorkflow={setSelectedWorkflow}
        />
        <div className="flex-1 flex flex-col min-w-0 overflow-hidden">
          <ConversationView
            claw={selectedClaw}
            allClaws={claws}
            downtimeDependencies={downtimeDependencies}
            messages={selectedClaw ? messages[selectedClaw.id] || [] : []}
            allMessages={messages}
            streamingBuffers={streamingBuffers}
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
      </div>
      <ManualTriggerModal
        open={!!selectedWorkflow}
        onOpenChange={(open) => { if (!open) setSelectedWorkflow(null) }}
        workflow={selectedWorkflow}
      />
    </div>
  )
}
