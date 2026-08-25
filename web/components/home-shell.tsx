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
import { currentTurnSubagents, subagentCounts, subagentSummaryLine } from "@/lib/subagents"
import { useNowMinuteTick } from "@/hooks/use-now"
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
import { Spinner } from "@/components/ui/spinner"

const AnalyticsCommandCenter = dynamic(() => import("@/components/analytics-command-center").then((module) => module.AnalyticsCommandCenter), {
  loading: () => <div className="flex min-h-64 items-center justify-center text-muted-foreground"><Spinner className="size-5" /></div>,
})

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
  const [currentUserResolved, setCurrentUserResolved] = useState(false)
  const [adminChecked, setAdminChecked] = useState(false)
  const [adminCheckFailed, setAdminCheckFailed] = useState(false)
  const lastAdminCheckAt = useRef(0)
  const [selectedWorkflow, setSelectedWorkflow] = useState<Workflow | null>(null)

  const loadAdminStatus = useCallback(async () => {
      const token = await requestAuthToken()
      if (!token) {
        setIsAdmin(false)
        setCurrentUserLogin(null)
        setCurrentUserResolved(false)
        setAdminChecked(true)
        setAdminCheckFailed(false)
        return false
      }
      const { getHubUrl } = await import("@/lib/hub-url")
      const hubUrl = getHubUrl()
      const url = hubUrl ? `${hubUrl}/api/auth/me` : "/api/auth/me"
      return fetch(url, { headers: { Authorization: `Bearer ${token}` } })
        .then(async (response) => {
          if (response.ok) return response.json()
          if (response.status === 401 || response.status === 403) throw new Error("Unauthorized")
          throw new Error("Unable to resolve current user")
        })
        .then(data => {
          setIsAdmin(data?.is_admin === true)
          setCurrentUserLogin(typeof data?.login === "string" ? data.login : null)
          setCurrentUserResolved(true)
          setAdminChecked(true)
          setAdminCheckFailed(false)
          return true
        })
        .catch((error) => {
          if (error instanceof Error && error.message === "Unauthorized") {
            setIsAdmin(false)
            setCurrentUserLogin(null)
            setCurrentUserResolved(false)
            setAdminChecked(true)
            setAdminCheckFailed(false)
            return true
          } else {
            setAdminChecked(false)
            setAdminCheckFailed(true)
            return false
          }
        })
  }, [])

  // Fetch admin status.
  useEffect(() => {
    let cancelled = false
    const guardedLoadAdminStatus = () => {
      if (lastAdminCheckAt.current && Date.now() - lastAdminCheckAt.current < 60_000) return
      lastAdminCheckAt.current = Date.now()
      void loadAdminStatus().then((succeeded) => {
        if (!succeeded) lastAdminCheckAt.current = 0
      }).catch(() => {
        lastAdminCheckAt.current = 0
        if (!cancelled) {
          setAdminChecked(false)
          setAdminCheckFailed(true)
        }
      })
    }
    guardedLoadAdminStatus()
    window.addEventListener("focus", guardedLoadAdminStatus)
    return () => { cancelled = true; window.removeEventListener("focus", guardedLoadAdminStatus) }
  }, [loadAdminStatus])

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

  // Sidebar third line: how many subagents the current turn is running. The
  // board's message map is a recent-activity *window*, so currentTurnSubagents
  // returns null whenever that window cannot reach the start of the turn — a
  // claw whose Task calls are still behind an unexpanded summary gets no line
  // at all rather than an undercount. See lib/subagents.ts.
  // A minute clock is plenty: the line counts running + quiet together, so the
  // 30s running/quiet split never changes what it says.
  const subagentNow = useNowMinuteTick(true)
  const calculatedSidebarSubagents = useMemo(() => {
    const lines: Record<string, string> = {}
    const now = subagentNow
    for (const claw of claws) {
      // Same liveness gate the activity line uses. statusForStep reads only
      // `endedAt`, so a Task start whose terminal never arrived (claw reaped or
      // killed mid-Task) stays "quiet" — i.e. counted as active — forever. And
      // once a turn is over, its tally would stay pinned to an idle claw's row
      // until the next user message, so the line only survives while the turn
      // is still moving.
      if (claw.status !== "connected" && !claw.isStreaming) continue
      const msgs = messages[claw.id]
      if (!msgs || msgs.length === 0) continue
      const subs = currentTurnSubagents(msgs, now)
      if (!subs || subs.length === 0) continue
      const { running, quiet } = subagentCounts(subs)
      if (!claw.isStreaming && running + quiet === 0) continue
      const line = subagentSummaryLine(subs)
      if (line) lines[claw.id] = line
    }
    return lines
  }, [claws, messages, subagentNow])
  const [sidebarSubagents, setSidebarSubagents] = useState(calculatedSidebarSubagents)
  if (Object.keys(sidebarSubagents).length !== Object.keys(calculatedSidebarSubagents).length || Object.keys(sidebarSubagents).some((id) => sidebarSubagents[id] !== calculatedSidebarSubagents[id])) {
    setSidebarSubagents(calculatedSidebarSubagents)
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
      subagentLines={sidebarSubagents}
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
          // Everyone reads analytics; only admins see the cost widgets, so the
          // view waits for the admin check instead of rendering a page whose
          // cost sections would pop in (or wrongly stay hidden) afterwards.
          adminChecked ? (
            <Suspense fallback={null}>
              <AnalyticsCommandCenter canViewCosts={isAdmin} />
            </Suspense>
          ) : adminCheckFailed ? (
            <div className="flex min-h-64 items-center justify-center p-6">
              <div className="max-w-sm rounded-lg border bg-card p-5 text-center">
                <p className="font-medium">Unable to check your access level.</p>
                <p className="mt-1 text-sm text-muted-foreground">Check your connection and try again.</p>
                <button type="button" className="mt-4 rounded-md bg-primary px-3 py-2 text-sm text-primary-foreground" onClick={() => void loadAdminStatus()}>Retry</button>
              </div>
            </div>
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
            currentUserResolved={currentUserResolved}
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
