"use client"

import { useState, useMemo, useEffect, useCallback, useRef, useSyncExternalStore, Suspense } from "react"
import dynamic from "next/dynamic"
import { usePathname } from "next/navigation"
import { Sidebar } from "@/components/sidebar"
import { ConversationView } from "@/components/conversation-view"
import { SetupScreen } from "@/components/setup-screen"
import { ManualTriggerModal } from "@/components/manual-trigger-modal"
import { MobileTabBar } from "@/components/mobile-tab-bar"
import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet"
import { useIsMobile } from "@/hooks/use-mobile"
import { useHub } from "@/hooks/use-hub"
import { useBoardActivityPrefetch } from "@/hooks/use-board-activity-prefetch"
import { demoteStaleRunning, pairActivitySteps, trailingActivityRun } from "@/lib/turns"
import { type Workflow } from "@/lib/api"
import {
  subscribeToShellStorage,
  getSelectedClawId,
  getServerSelectedClawId,
  writeSelectedClawId,
  getConfigured,
  getServerConfigured,
  markConfigured,
} from "@/lib/shell-storage"
import { requestAuthToken } from "@/lib/auth-storage"
import { isWaitingOnYou } from "@/lib/waiting-on-you"
import { agentSection, type AgentSectionName } from "@/components/ds/agent-section"

const AnalyticsCommandCenter = dynamic(() => import("@/components/analytics-command-center").then((module) => module.AnalyticsCommandCenter))

export type HomeView = "agents" | "analytics"

/**
 * Shell shared by "/" and "/analytics": one sidebar, one hub connection, and a
 * right-hand pane that swaps between the agents board and analytics. The view
 * is derived from the pathname; toggling uses history.pushState so the app
 * never remounts (Next syncs usePathname/useSearchParams with the history API)
 * and the hub WebSocket stays alive. Both static-export routes render this
 * same component so deep links to either path hydrate into the same shell.
 */
export function HomeShell() {
  const pathname = usePathname()
  const view: HomeView = pathname.startsWith("/analytics") ? "analytics" : "agents"
  // Last analytics query string, so toggling away and back restores filters.
  const analyticsSearchRef = useRef("")

  // Storage-backed state comes from an external store, never from a useState
  // initializer: reading localStorage during the first client render makes it
  // disagree with the SSR pass and React throws away the tree.
  const selectedClawId = useSyncExternalStore(
    subscribeToShellStorage,
    getSelectedClawId,
    getServerSelectedClawId
  )
  const [searchQuery, setSearchQuery] = useState("")
  const [activeTagFilters, setActiveTagFilters] = useState<string[]>([])
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  const handleToggleSidebarCollapse = useCallback(() => {
    setSidebarCollapsed((collapsed) => !collapsed)
  }, [])
  // Below 768px the sidebar becomes a slide-over drawer opened from the
  // board header hamburger; the footer destinations move to a bottom tab bar.
  const isMobile = useIsMobile()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const configuredState = useSyncExternalStore(
    subscribeToShellStorage,
    getConfigured,
    getServerConfigured
  )
  const [isAdmin, setIsAdmin] = useState(false)
  const [currentUserLogin, setCurrentUserLogin] = useState<string | null>(null)
  const [adminChecked, setAdminChecked] = useState(false)
  const [selectedWorkflow, setSelectedWorkflow] = useState<Workflow | null>(null)

  // Fetch admin status
  useEffect(() => {
    let cancelled = false
    async function loadAdminStatus() {
      const token = await requestAuthToken()
      if (cancelled) return
      if (!token) {
        setIsAdmin(false)
        setAdminChecked(true)
        return
      }
      const { getHubUrl } = await import("@/lib/hub-url")
      if (cancelled) return
      const hubUrl = getHubUrl()
      const url = hubUrl ? `${hubUrl}/api/auth/me` : "/api/auth/me"
      fetch(url, { headers: { Authorization: `Bearer ${token}` } })
        .then(r => r.ok ? r.json() : null)
        .then(data => {
          if (cancelled) return
          setIsAdmin(data?.is_admin === true)
          setCurrentUserLogin(typeof data?.login === "string" ? data.login : null)
          setAdminChecked(true)
        })
        .catch(() => {
          if (cancelled) return
          setIsAdmin(false)
          setCurrentUserLogin(null)
          setAdminChecked(true)
        })
    }
    loadAdminStatus()
    return () => { cancelled = true }
  }, [])

  // Non-admins never see analytics: deep links bounce back to the agents view.
  useEffect(() => {
    if (view === "analytics" && adminChecked && !isAdmin) {
      window.history.replaceState(null, "", "/")
    }
  }, [view, adminChecked, isAdmin])

  const handleToggleView = useCallback(() => {
    if (view === "analytics") {
      analyticsSearchRef.current = window.location.search
      window.history.pushState(null, "", "/")
    } else {
      window.history.pushState(null, "", `/analytics/${analyticsSearchRef.current}`)
    }
  }, [view])

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
    prefetchTrailingActivity,
    setUnreadCount,
    setPinned,
    send,
    killClaw,
  } = hub

  const claws = rawClaws
  const allClawIds = useMemo(() => claws.map((claw) => claw.id), [claws])

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
  const matchesSearchQuery = useCallback((claw: typeof claws[number]) => {
    const query = searchQuery.trim().toLowerCase()
    return !query || claw.name.toLowerCase().includes(query) || claw.tags.some((tag) => tag.toLowerCase().includes(query))
  }, [searchQuery])

  const filteredClaws = useMemo(() => {
    let result = searchQuery.trim() ? claws : unpinnedClaws

    if (searchQuery.trim()) {
      result = result.filter(matchesSearchQuery)
    }

    if (activeTagFilters.length > 0) {
      result = result.filter((c) =>
        activeTagFilters.every((tag) => c.tags.includes(tag))
      )
    }

    return result
  }, [claws, unpinnedClaws, searchQuery, activeTagFilters, matchesSearchQuery])

  const filteredPinnedClaws = useMemo(() => {
    return pinnedClaws.filter((claw) =>
      matchesSearchQuery(claw) && activeTagFilters.every((tag) => claw.tags.includes(tag))
    )
  }, [pinnedClaws, activeTagFilters, matchesSearchQuery])

  // Keep streamed message churn out of Sidebar: it only needs each claw's
  // resolved section, not the complete transcript map.
  // Recomputed per message update, but identity-stable when nothing changed:
  // a fresh Map per WS chunk would re-render every sidebar row.
  const calculatedSidebarSections = useMemo(() => {
    const sections = new Map<string, AgentSectionName>()
    for (const claw of claws) {
      sections.set(claw.id, agentSection(claw, { isWaitingOnYou: isWaitingOnYou(messages[claw.id] ?? []) }))
    }
    return sections
  }, [claws, messages])
  const [sidebarSections, setSidebarSections] = useState(calculatedSidebarSections)
  if (sidebarSections.size !== calculatedSidebarSections.size || [...sidebarSections].some(([id, section]) => calculatedSidebarSections.get(id) !== section)) {
    setSidebarSections(calculatedSidebarSections)
  }

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

  // Board cards need step rows without interaction: as each timeline lands,
  // expand its trailing activity summaries (bounded + concurrency-capped).
  useBoardActivityPrefetch({
    active: view === "agents" && !selectedClawId,
    claws,
    messages,
    prefetch: prefetchTrailingActivity,
  })

  // Sidebar second line: what each active claw is running right now. Only the
  // trailing activity run is scanned, so this stays cheap per message update.
  const calculatedSidebarActivity = useMemo(() => {
    const lines: Record<string, string> = {}
    for (const claw of claws) {
      if (claw.status !== "connected" && !claw.isStreaming) continue
      const msgs = messages[claw.id]
      if (!msgs || msgs.length === 0) continue
      const steps = demoteStaleRunning(pairActivitySteps(trailingActivityRun(msgs)), true)
      const last = steps[steps.length - 1]
      if (last && last.status === "running") {
        lines[claw.id] = last.detail ? `${last.title} · ${last.detail}` : last.title
      } else if (claw.isStreaming) {
        lines[claw.id] = "Writing a reply"
      }
    }
    return lines
  }, [claws, messages])
  const [sidebarActivity, setSidebarActivity] = useState(calculatedSidebarActivity)
  if (Object.keys(sidebarActivity).length !== Object.keys(calculatedSidebarActivity).length || Object.keys(sidebarActivity).some((id) => sidebarActivity[id] !== calculatedSidebarActivity[id])) {
    setSidebarActivity(calculatedSidebarActivity)
  }

  // Mark messages as read when selecting a claw + lazy load history
  const handleSelectClaw = useCallback(
    (id: string) => {
      // Selecting an agent while on analytics jumps back to the agents view.
      if (window.location.pathname.startsWith("/analytics")) {
        analyticsSearchRef.current = window.location.search
        window.history.pushState(null, "", "/")
      }
      writeSelectedClawId(id)
      setUnreadCount(id, 0)
      loadMessages(id)
      setDrawerOpen(false)
    },
    [loadMessages, setUnreadCount]
  )

  // When in board view (no claw selected), all cards are visible — clear unread
  // for any claw that has messages showing on screen. Analytics shares this
  // shell with selectedClawId === null but shows no cards, so it must not
  // silently mark replies as read.
  useEffect(() => {
    if (view !== "agents" || selectedClawId) return
    const withUnread = claws.filter((c) => c.unreadCount > 0)
    if (withUnread.length === 0) return
    for (const c of withUnread) {
      setUnreadCount(c.id, 0)
    }
  }, [view, selectedClawId, claws, setUnreadCount])

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
    writeSelectedClawId(null)
  }, [selectedClawId, killClaw])

  const handleKillClaw = useCallback(
    (clawId: string) => {
      killClaw(clawId)
      if (selectedClawId === clawId) {
        writeSelectedClawId(null)
      }
    },
    [selectedClawId, killClaw]
  )

  // Show loading state until we know if configured
  if (configuredState === null) {
    return <div className="flex h-screen-safe bg-background items-center justify-center" />
  }

  // Show setup screen if not configured
  if (!configuredState) {
    return <SetupScreen onConnected={markConfigured} />
  }

  const sidebar = (
    <Sidebar
      claws={filteredClaws}
      pinnedClaws={filteredPinnedClaws}
      allClawIds={allClawIds}
      agentSections={sidebarSections}
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
      isCollapsed={!isMobile && sidebarCollapsed}
      onToggleCollapse={handleToggleSidebarCollapse}
      onReorderClaws={reorderClaws}
      activityLines={sidebarActivity}
      isAdmin={isAdmin}
      onSelectWorkflow={setSelectedWorkflow}
      view={view}
      onToggleView={handleToggleView}
      variant={isMobile ? "drawer" : "inline"}
    />
  )

  // The tab bar shows on the board and analytics; the agent detail view is
  // full-screen (back chevron returns to the board).
  const showTabBar = isMobile && (view === "analytics" || !selectedClaw)

  return (
    <div className="flex h-screen-safe bg-background">
      {isMobile ? (
        <Sheet open={drawerOpen} onOpenChange={setDrawerOpen}>
          <SheetContent side="left" className="w-[85vw] max-w-[320px] gap-0 p-0">
            <SheetTitle className="sr-only">Agents</SheetTitle>
            {sidebar}
          </SheetContent>
        </Sheet>
      ) : (
        sidebar
      )}
      <div className="flex-1 flex flex-col min-w-0 overflow-hidden">
        {view === "analytics" ? (
          isAdmin ? (
            <Suspense fallback={null}>
              <AnalyticsCommandCenter />
            </Suspense>
          ) : null
        ) : (
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
            onDeselectClaw={() => writeSelectedClawId(null)}
            onReorderClaws={reorderClaws}
            loading={loading}
            hubError={hubError}
            currentUserLogin={currentUserLogin}
            currentUserResolved={adminChecked}
            onOpenMenu={isMobile ? () => setDrawerOpen(true) : undefined}
          />
        )}
        {showTabBar && (
          <MobileTabBar
            view={view}
            isAdmin={isAdmin}
            onSelectAgents={() => { if (view === "analytics") handleToggleView() }}
            onSelectAnalytics={() => { if (view === "agents") handleToggleView() }}
          />
        )}
      </div>
      <ManualTriggerModal
        open={!!selectedWorkflow}
        onOpenChange={(open) => { if (!open) setSelectedWorkflow(null) }}
        workflow={selectedWorkflow}
      />
    </div>
  )
}
