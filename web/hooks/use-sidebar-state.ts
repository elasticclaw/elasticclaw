"use client"

import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { requestAuthToken } from "@/lib/auth-storage"
import { useHub } from "@/hooks/use-hub"

const SELECTED_CLAW_KEY = "elasticclaw_selected_claw"

export function useSidebarState() {
  const [selectedClawId, setSelectedClawId] = useState<string | null>(() => {
    if (typeof window === "undefined") return null
    return localStorage.getItem(SELECTED_CLAW_KEY) ?? null
  })
  const [searchQuery, setSearchQuery] = useState("")
  const [activeTagFilters, setActiveTagFilters] = useState<string[]>([])
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  const [isAdmin, setIsAdmin] = useState(false)

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
  const { claws, streamingBuffers, reorderClaws } = hub

  const displayClaws = useMemo(() =>
    claws.map((c) => ({
      ...c,
      isStreaming: c.isStreaming || Boolean(streamingBuffers[c.id]?.hadChunks && streamingBuffers[c.id]?.text),
    })),
    [claws, streamingBuffers]
  )

  const allTags = useMemo(() => {
    const tagSet = new Set<string>()
    displayClaws.forEach((claw) => claw.tags.forEach((tag) => tagSet.add(tag)))
    return Array.from(tagSet).sort()
  }, [displayClaws])
  const pinnedClaws = useMemo(() => displayClaws.filter((claw) => claw.pinned), [displayClaws])
  const unpinnedClaws = useMemo(() => displayClaws.filter((claw) => !claw.pinned), [displayClaws])
  const filteredClaws = useMemo(() => {
    let result = searchQuery.trim() ? displayClaws : unpinnedClaws
    if (searchQuery.trim()) {
      const query = searchQuery.toLowerCase()
      result = result.filter((claw) => claw.name.toLowerCase().includes(query) || claw.tags.some((tag) => tag.toLowerCase().includes(query)))
    }
    if (activeTagFilters.length > 0) {
      result = result.filter((claw) => activeTagFilters.every((tag) => claw.tags.includes(tag)))
    }
    return result
  }, [displayClaws, unpinnedClaws, searchQuery, activeTagFilters])
  const filteredPinnedClaws = useMemo(() => {
    if (activeTagFilters.length === 0) return pinnedClaws
    return pinnedClaws.filter((claw) => activeTagFilters.every((tag) => claw.tags.includes(tag)))
  }, [pinnedClaws, activeTagFilters])

  const boardLoadedRef = useRef(false)
  useEffect(() => {
    if (boardLoadedRef.current || displayClaws.length === 0) return
    boardLoadedRef.current = true
    for (const claw of displayClaws) hub.loadMessages(claw.id)
  }, [displayClaws, hub])

  const handleSelectClaw = useCallback((id: string) => {
    setSelectedClawId(id)
    localStorage.setItem(SELECTED_CLAW_KEY, id)
    hub.setUnreadCount(id, 0)
    hub.loadMessages(id)
  }, [hub])
  const handleTogglePin = useCallback((id: string) => {
    const claw = displayClaws.find((item) => item.id === id)
    if (claw) hub.setPinned(id, !claw.pinned)
  }, [displayClaws, hub])
  const handleAddTagFilter = useCallback((tag: string) => setActiveTagFilters((prev) => prev.includes(tag) ? prev : [...prev, tag]), [])
  const handleRemoveTagFilter = useCallback((tag: string) => setActiveTagFilters((prev) => prev.filter((item) => item !== tag)), [])
  const handleClearTagFilters = useCallback(() => setActiveTagFilters([]), [])

  useEffect(() => {
    if (selectedClawId) return
    const withUnread = displayClaws.filter((claw) => claw.unreadCount > 0)
    for (const claw of withUnread) hub.setUnreadCount(claw.id, 0)
  }, [selectedClawId, displayClaws, hub])

  return {
    hub,
    claws: displayClaws,
    selectedClawId,
    setSelectedClawId,
    sidebarProps: {
      claws: filteredClaws,
      pinnedClaws: filteredPinnedClaws,
      allClawIds: displayClaws.map((claw) => claw.id),
      selectedClawId,
      onSelectClaw: handleSelectClaw,
      onTogglePin: handleTogglePin,
      searchQuery,
      onSearchChange: setSearchQuery,
      allTags,
      activeTagFilters,
      onAddTagFilter: handleAddTagFilter,
      onRemoveTagFilter: handleRemoveTagFilter,
      onClearTagFilters: handleClearTagFilters,
      isCollapsed: sidebarCollapsed,
      onToggleCollapse: () => setSidebarCollapsed(!sidebarCollapsed),
      onReorderClaws: reorderClaws,
      isAdmin,
    },
  }
}

export type SidebarState = ReturnType<typeof useSidebarState>
export { SELECTED_CLAW_KEY }
