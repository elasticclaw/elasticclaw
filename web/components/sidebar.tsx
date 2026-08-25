"use client"

import { Search, X, ChevronDown, PanelLeftClose, PanelLeft, Loader2, AlertCircle, LogOut, Settings, Plus, BarChart3, LayoutGrid } from "lucide-react"
import { useBranding } from "@/hooks/use-branding"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { ScrollArea } from "@/components/ui/scroll-area"
import { ClawCard } from "@/components/claw-card"
import { WorkflowName } from "@/components/workflow-name"
import { clearConfig, fetchWorkspaces, type Workflow } from "@/lib/api"
import { signOut } from "@/lib/sign-out"
import { useIsMobile } from "@/hooks/use-mobile"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { cn } from "@/lib/utils"
import type { Claw } from "@/lib/types"
import { AgentStateChip } from "@/components/ds/agent-state-chip"
import { AGENT_SECTION, agentSection, type AgentSectionName } from "@/components/ds/agent-section"
import {
  DndContext,
  closestCenter,
  PointerSensor,
  useSensor,
  useSensors,
  DragEndEvent,
  DragOverlay,
  DragStartEvent,
  CollisionDetection,
} from "@dnd-kit/core"
import {
  SortableContext,
  useSortable,
  verticalListSortingStrategy,
  arrayMove,
} from "@dnd-kit/sortable"
import { CSS } from "@dnd-kit/utilities"
import { memo, useState, useEffect, useCallback, useMemo } from "react"

type TagFilter = string

interface SidebarProps {
  claws: Claw[]
  pinnedClaws: Claw[]
  allClawIds: string[]
  agentSections: Map<string, AgentSectionName>
  selectedClawId: string | null
  onSelectClaw: (id: string) => void
  onTogglePin: (id: string) => void
  onReorderClaws: (ids: string[]) => void
  searchQuery: string
  onSearchChange: (query: string) => void
  allTags: string[]
  activeTagFilters: TagFilter[]
  onAddTagFilter: (filter: TagFilter) => void
  onRemoveTagFilter: (filter: TagFilter) => void
  onClearTagFilters: () => void
  isCollapsed: boolean
  onToggleCollapse: () => void
  /** Per-claw "current tool" one-liners for the second row line. */
  activityLines?: Record<string, string>
  /**
   * Per-claw subagent one-liners. Absent for any claw whose loaded message
   * window cannot cover the current turn — the count would undercount, so the
   * line is omitted rather than guessed (see lib/subagents.ts).
   */
  subagentLines?: Record<string, string>
  isAdmin?: boolean
  onSelectWorkflow?: (workflow: Workflow | null) => void
  view?: "agents" | "analytics"
  onToggleView?: () => void
  /**
   * "inline" is the desktop column; "drawer" renders inside the mobile Sheet:
   * full-size, no collapse toggle, and no footer (the bottom tab bar and the
   * board header menu own those destinations on mobile).
   */
  variant?: "inline" | "drawer"
}

/** Thin wrapper that gives ClawCard sortable DnD powers */
const SortableClawCard = memo(function SortableClawCard({
  claw,
  isSelected,
  onSelectClaw,
  onTogglePin,
  activityLine,
  subagentLine,
}: {
  claw: Claw
  isSelected: boolean
  onSelectClaw: (id: string) => void
  onTogglePin: (id: string) => void
  activityLine?: string
  subagentLine?: string
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } =
    useSortable({ id: claw.id })

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.4 : 1,
    cursor: isDragging ? "grabbing" : undefined,
  }
  const handleClick = useCallback(() => onSelectClaw(claw.id), [claw.id, onSelectClaw])
  const handleTogglePin = useCallback((event: React.MouseEvent) => { event.stopPropagation(); onTogglePin(claw.id) }, [claw.id, onTogglePin])
  const stateChip = useMemo(() => <AgentStateChip status={claw.status} isStreaming={claw.isStreaming} />, [claw.status, claw.isStreaming])

  return (
    <div ref={setNodeRef} style={style} {...attributes} {...listeners} className="w-full min-w-0">
      <ClawCard
        claw={claw}
        isSelected={isSelected}
        onClick={handleClick}
        onTogglePin={handleTogglePin}
        activityLine={activityLine}
        subagentLine={subagentLine}
        stateChip={stateChip}
      />
    </div>
  )
})

export const Sidebar = memo(function Sidebar({
  claws,
  pinnedClaws,
  allClawIds,
  agentSections,
  selectedClawId,
  onSelectClaw,
  onTogglePin,
  onReorderClaws,
  searchQuery,
  onSearchChange,
  allTags,
  activeTagFilters,
  onAddTagFilter,
  onRemoveTagFilter,
  onClearTagFilters,
  isCollapsed,
  onToggleCollapse,
  activityLines,
  subagentLines,
  isAdmin = true,
  onSelectWorkflow,
  view = "agents",
  onToggleView,
  variant = "inline",
}: SidebarProps) {
  const inDrawer = variant === "drawer"
  const tagKeys = allTags
  const { appName } = useBranding()
  const [activeDragClaw, setActiveDragClaw] = useState<Claw | null>(null)
  const [manualWorkflows, setManualWorkflows] = useState<Workflow[]>([])
  const [showWorkflowPicker, setShowWorkflowPicker] = useState(false)
  const [loadingManualWorkflows, setLoadingManualWorkflows] = useState(false)

  const fetchManualWorkflows = useCallback(async () => {
    const data = await fetchWorkspaces()
    const workflows = data.flatMap((workspace) => workspace.workflows || [])
    return workflows.filter((workflow) => workflow.enableManualTrigger)
  }, [])

  const openWorkflowLauncher = useCallback(async () => {
    setLoadingManualWorkflows(true)
    try {
      const workflows = await fetchManualWorkflows()
      setManualWorkflows(workflows)
      if (workflows.length === 1) {
        onSelectWorkflow?.(workflows[0])
      } else if (workflows.length > 1) {
        setShowWorkflowPicker(true)
      } else {
        setShowWorkflowPicker(true)
      }
    } catch (err) {
      console.error("[sidebar] fetchWorkflows error:", err)
      if (manualWorkflows.length === 1) {
        onSelectWorkflow?.(manualWorkflows[0])
      } else if (manualWorkflows.length > 1) {
        setShowWorkflowPicker(true)
      }
    } finally {
      setLoadingManualWorkflows(false)
    }
  }, [fetchManualWorkflows, manualWorkflows, onSelectWorkflow])

  // Warm the manual-trigger workflow cache from persisted workspaces.
  useEffect(() => {
    let cancelled = false
    let attempts = 0
    const load = async () => {
      try {
        const manual = await fetchManualWorkflows()
        if (cancelled) return
        setManualWorkflows(manual)
        console.log("[sidebar] loaded manual workflows:", manual.length)
      } catch (err) {
        if (cancelled) return
        console.error("[sidebar] fetchWorkflows error (attempt", attempts + 1, "):", err)
        attempts++
        if (attempts < 3) {
          setTimeout(load, attempts * 500)
        }
      }
    }
    load()
    return () => { cancelled = true }
  }, [claws.length, fetchManualWorkflows]) // re-check when claws change (new claw from trigger)

  const handleSignOut = useCallback(() => signOut(), [])

  // The view toggle row points at the destination: it reads "Analytics" while
  // on the agents board and "Agents" while on analytics.
  const inAnalytics = view === "analytics"
  const viewToggleLabel = inAnalytics ? "Agents" : "Analytics"
  const ViewToggleIcon = inAnalytics ? LayoutGrid : BarChart3

  // On touch/mobile a 6px activation would steal one-finger scrolling, so
  // reordering requires a press-and-hold instead. Desktop keeps the 6px
  // distance so clicks pass through.
  const isMobile = useIsMobile()
  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: isMobile
        ? { delay: 350, tolerance: 8 }
        : { distance: 6 },
    })
  )

  function handleDragStart(event: DragStartEvent) {
    const allVisible = [...pinnedClaws, ...claws]
    const found = allVisible.find((c) => c.id === event.active.id)
    setActiveDragClaw(found ?? null)
  }

  // Pinned and unpinned agents share a section's SortableContext, but reordering
  // across that boundary is rejected in handleDragEnd. Left to dnd-kit's default
  // collision detection, the list still previews the move while dragging and only
  // snaps back on drop — a confusing refusal with no visible cue. Filtering out
  // cross-pinned drop targets up front keeps the list from moving at all when the
  // drag can't succeed, so there's nothing to snap back from.
  const collisionDetectionForClaws: CollisionDetection = useCallback((args) => {
    const activeClaw = [...pinnedClaws, ...claws].find((claw) => claw.id === args.active.id)
    if (!activeClaw) return closestCenter(args)
    const activePinned = pinnedClaws.some((claw) => claw.id === args.active.id)
    const sectionFor = (candidate: Claw) => agentSections.get(candidate.id) ?? agentSection(candidate, { isWaitingOnYou: false })
    const activeSection = sectionFor(activeClaw)
    const allowed = args.droppableContainers.filter((container) => {
      const overClaw = [...pinnedClaws, ...claws].find((claw) => claw.id === container.id)
      return container.id === args.active.id || Boolean(overClaw && sectionFor(overClaw) === activeSection && pinnedClaws.some((claw) => claw.id === overClaw.id) === activePinned)
    })
    return closestCenter({ ...args, droppableContainers: allowed })
  }, [agentSections, claws, pinnedClaws])

  function handleDragEnd(event: DragEndEvent) {
    setActiveDragClaw(null)
    const { active, over } = event
    if (!over || active.id === over.id) return

    const visible = [...pinnedClaws, ...claws.filter((c) => !pinnedClaws.some((p) => p.id === c.id))]
    const activeClaw = visible.find((c) => c.id === active.id)
    const overClaw = visible.find((c) => c.id === over.id)
    if (!activeClaw || !overClaw) return
    const sectionFor = (candidate: Claw) => agentSections.get(candidate.id) ?? agentSection(candidate, { isWaitingOnYou: false })
    const section = sectionFor(activeClaw)
    if (section !== sectionFor(overClaw)) return
    if (activeClaw.pinned !== overClaw.pinned) return

    const ids = visible.filter((candidate) => sectionFor(candidate) === section).map((candidate) => candidate.id)
    const reordered = arrayMove(ids, ids.indexOf(active.id as string), ids.indexOf(over.id as string))
    const order = allClawIds.slice()
    let next = 0
    const rewritten = order.map((id) => ids.includes(id) ? reordered[next++] : id)
    onReorderClaws(rewritten)
  }

  // Merge pinned + unpinned for collapsed view (order already applied by parent)
  const allClaws = useMemo(
    () => [...pinnedClaws, ...claws.filter(c => !pinnedClaws.find(p => p.id === c.id))],
    [pinnedClaws, claws]
  )

  // The collapsed rail keeps the expanded list's grouping: same section order,
  // same pinned-first sort — collapsing must not reshuffle the agents.
  const sections = useMemo(() => (["attention", "working", "offline"] as AgentSectionName[])
    .map((key) => ({
      key,
      meta: AGENT_SECTION[key],
      items: allClaws
        .filter((candidate) => agentSections.get(candidate.id) === key)
        .sort((a, b) => Number(b.pinned) - Number(a.pinned)),
    }))
    // ids computed here so SortableContext gets a stable array — a fresh one
    // per render re-renders every useSortable row through context.
    .map((section) => ({ ...section, ids: section.items.map((candidate) => candidate.id) })), [allClaws, agentSections])
  const collapsedSections = useMemo(() => sections.filter((section) => section.items.length > 0), [sections])

  let sidebar: React.ReactElement

  if (isCollapsed && !inDrawer) {
    sidebar = (
      <aside className="w-12 h-screen-safe flex flex-col border-r border-border bg-card">
        {/* py matches the board header so the rail's hairline meets it at the
            same height; Create Agent moves below the line to keep it that way. */}
        <div className="px-2 py-2.5 border-b border-border flex flex-col items-center">
          <Button
            variant="ghost"
            size="icon"
            onClick={onToggleCollapse}
            className="size-8"
            title="Expand sidebar"
          >
            <PanelLeft className="size-4" />
          </Button>
        </div>
        <div className="flex flex-col items-center gap-1 py-2 overflow-y-auto flex-1 min-h-0">
          {manualWorkflows.length > 0 && (
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              title="Create Agent"
              onClick={openWorkflowLauncher}
              disabled={loadingManualWorkflows}
            >
              {loadingManualWorkflows ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
            </Button>
          )}
          {collapsedSections.map((section) => (
            <div key={section.key} className="flex flex-col items-center gap-1">
              {/* Section marker: a short bar in the section color, standing in
                  for the expanded header. */}
              <span
                className="mt-1 h-0.5 w-6 rounded-full"
                title={`${section.meta.label} (${section.items.length})`}
                style={{ backgroundColor: `color-mix(in srgb, ${section.meta.color} 60%, transparent)` }}
              />
              {section.items.map((claw) => {
            const isSelected = claw.id === selectedClawId
            const hasUnread = claw.unreadCount > 0
            return (
              <button
                key={claw.id}
                onClick={() => onSelectClaw(claw.id)}
                title={claw.name}
                className={cn(
                  "relative size-8 rounded-md flex items-center justify-center transition-colors hover:bg-accent",
                  isSelected && "bg-accent"
                )}
              >
                {/* Status dot */}
                {claw.status === "provisioning" ? (
                  <Loader2 className="size-3.5 animate-spin" style={{ color: "var(--status-provisioning)" }} />
                ) : claw.status === "error" ? (
                  <AlertCircle className="size-3.5" style={{ color: "var(--status-error)" }} />
                ) : claw.isStreaming ? (
                  <Loader2 className="size-3.5 animate-spin" style={{ color: "var(--status-streaming)" }} />
                ) : (
                  <span className="size-2.5 rounded-full" style={{ backgroundColor: claw.status === "connected" ? "var(--status-connected)" : claw.status === "idle" ? "var(--status-idle)" : "var(--status-offline)" }} />
                )}
                {/* Unread badge */}
                {hasUnread && (
                  <span className="absolute -top-0.5 -right-0.5 size-3.5 flex items-center justify-center text-[8px] font-bold bg-blue-600 text-white rounded-full">
                    {claw.unreadCount > 9 ? "9+" : claw.unreadCount}
                  </span>
                )}
              </button>
            )
          })}
            </div>
          ))}
        </div>

        {/* Footer: view toggle (everyone) + settings (admin) and sign out */}
        <div className="p-2 border-t border-border flex flex-col items-center gap-1">
          <Button
            variant="ghost"
            size="icon"
            className={cn(
              "size-8 text-muted-foreground hover:text-foreground",
              inAnalytics && "bg-muted text-foreground"
            )}
            title={viewToggleLabel}
            onClick={onToggleView}
          >
            <ViewToggleIcon className="size-4" />
          </Button>
          {isAdmin && (
            <Button
              variant="ghost"
              size="icon"
              className="size-8 text-muted-foreground hover:text-foreground"
              title="Settings"
              onClick={() => { window.location.href = "/settings" }}
            >
              <Settings className="size-4" />
            </Button>
          )}
          <div className="my-0.5 w-6 border-t border-border" />
          <Button
            variant="ghost"
            size="icon"
            className="size-8 text-muted-foreground hover:text-destructive hover:bg-destructive/10"
            title="Sign out"
            onClick={handleSignOut}
          >
            <LogOut className="size-4" />
          </Button>
        </div>
      </aside>
    )
  } else {
    sidebar = (
      <aside
        className={cn(
          "flex flex-col bg-card",
          inDrawer ? "w-full h-full" : "w-[260px] h-screen-safe border-r border-border"
        )}
      >
      {/* py matches the board header so the two hairlines meet at the same height. */}
      <div className={cn("flex items-center justify-between px-4 py-2.5 border-b border-border", inDrawer && "pr-12")}>
        <h1 className="text-lg font-semibold tracking-tight text-foreground">
          {appName}
        </h1>
        <div className="flex items-center gap-1">
          {manualWorkflows.length > 0 && (
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              title="Create Agent"
              onClick={openWorkflowLauncher}
              disabled={loadingManualWorkflows}
            >
              {loadingManualWorkflows ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
            </Button>
          )}
          {!inDrawer && (
            <Button
              variant="ghost"
              size="icon"
              onClick={onToggleCollapse}
              className="size-8"
              title="Collapse sidebar"
            >
              <PanelLeftClose className="size-4" />
            </Button>
          )}
        </div>
      </div>

      <div className="p-3 border-b border-border space-y-2">
        <div className="relative">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 size-4 text-muted-foreground" />
          <Input
            placeholder="Filter by name or workflow..."
            value={searchQuery}
            onChange={(e) => onSearchChange(e.target.value)}
            className="pl-8 h-8 text-sm bg-background"
          />
        </div>

        <div className="flex items-center gap-1.5 flex-wrap">
          {tagKeys.length > 0 && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 text-xs gap-1">
                  <span>Tags</span>
                  <ChevronDown className="size-3" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start" className="w-48">
                {tagKeys.map((tag) => {
                  const isActive = activeTagFilters.includes(tag)
                  return (
                    <DropdownMenuItem
                      key={tag}
                      className="text-xs"
                      onClick={() => isActive ? onRemoveTagFilter(tag) : onAddTagFilter(tag)}
                    >
                      <span className={isActive ? "font-medium" : ""}>{tag}</span>
                      {isActive && <span className="ml-auto text-muted-foreground">✓</span>}
                    </DropdownMenuItem>
                  )
                })}
              </DropdownMenuContent>
            </DropdownMenu>
          )}
          
          {activeTagFilters.map((tag) => (
            <span
              key={tag}
              title={tag}
              className="inline-flex items-center gap-1 px-2 py-1 text-xs bg-secondary text-foreground rounded max-w-[120px]"
            >
              <span className="truncate">{tag}</span>
              <button
                onClick={() => onRemoveTagFilter(tag)}
                className="ml-0.5 hover:text-destructive flex-shrink-0"
              >
                <X className="size-3" />
              </button>
            </span>
          ))}
          
          {activeTagFilters.length > 1 && (
            <button
              onClick={onClearTagFilters}
              className="text-xs text-muted-foreground hover:text-foreground"
            >
              Clear all
            </button>
          )}
        </div>
      </div>

      <DndContext
        sensors={sensors}
        collisionDetection={collisionDetectionForClaws}
        onDragStart={handleDragStart}
        onDragEnd={handleDragEnd}
      >
        {/* Pinned section lives inside the scroll area — a long pinned list
            must scroll with the rest instead of squeezing "All Agents" out. */}
        <ScrollArea className="flex-1 min-h-0 min-w-0 overflow-x-hidden [&>[data-slot=scroll-area-viewport]>div]:!block [&>[data-slot=scroll-area-viewport]>div]:!w-full [&>[data-slot=scroll-area-viewport]>div]:!min-w-0">
          <div className="w-full min-w-0 overflow-hidden">
            {allClaws.length === 0 ? (
              <p className="text-sm text-muted-foreground text-center py-8">
                No agents found
              </p>
            ) : (
              sections.map(({ key, meta, items, ids }) => {
                const Icon = meta.icon
                return <section key={key} className="min-w-0 pb-1">
                  <div className="sticky top-0 left-0 right-0 z-10 flex w-full min-w-0 items-center gap-2 border-y px-3 py-1.5" style={{ backgroundColor: `color-mix(in srgb, ${meta.color} 10%, var(--card))`, borderColor: `color-mix(in srgb, ${meta.color} 30%, var(--border))` }}>
                    <span className="w-[3px] self-stretch rounded-full" style={{ backgroundColor: meta.color }} />
                    <Icon className="size-[13px]" style={{ color: meta.color }} />
                    <span className="min-w-0 truncate text-xs font-semibold uppercase tracking-[0.08em]">{meta.label}</span>
                    <span className="ml-auto shrink-0 rounded-full px-1.5 font-mono text-[10px] font-medium" style={{ backgroundColor: `color-mix(in srgb, ${meta.color} 22%, transparent)`, color: meta.color }}>{items.length}</span>
                  </div>
                  <div className="space-y-1 px-2 py-2">
                    {items.length === 0 ? <p className="px-2 py-1 text-xs text-muted-foreground">{meta.empty}</p> :
                      <SortableContext items={ids} strategy={verticalListSortingStrategy}>
                        {items.map((candidate) => <SortableClawCard key={candidate.id} claw={candidate} isSelected={candidate.id === selectedClawId} onSelectClaw={onSelectClaw} onTogglePin={onTogglePin} activityLine={activityLines?.[candidate.id]} subagentLine={subagentLines?.[candidate.id]} />)}
                      </SortableContext>}
                  </div>
                </section>
              })
            )}
          </div>
        </ScrollArea>

        {/* Drag overlay — ghost card that follows the cursor */}
        <DragOverlay>
          {activeDragClaw ? (
            <div className="opacity-90 shadow-xl">
              <ClawCard
                claw={activeDragClaw}
                isSelected={activeDragClaw.id === selectedClawId}
                onClick={() => {}}
                onTogglePin={() => {}}
                showPinButton={false}
              />
            </div>
          ) : null}
        </DragOverlay>
      </DndContext>

      {/* Footer: view toggle (everyone) + settings (admin) and sign out. Hidden in the
          mobile drawer — the bottom tab bar and the board header menu own
          these destinations there. */}
      {!inDrawer && (
      <div className="p-2 border-t border-border">
        <button
          onClick={onToggleView}
          className={cn(
            "flex w-full min-h-9 items-center gap-[9px] rounded-lg px-2.5 text-[12.5px] text-muted-foreground transition-colors hover:bg-muted hover:text-foreground",
            inAnalytics && "bg-muted text-foreground"
          )}
        >
          <ViewToggleIcon className="size-4 flex-shrink-0" />
          {viewToggleLabel}
        </button>
        {isAdmin && (
          <button
            onClick={() => { window.location.href = "/settings" }}
            className="flex w-full min-h-9 items-center gap-[9px] rounded-lg px-2.5 text-[12.5px] text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
          >
            <Settings className="size-4 flex-shrink-0" />
            Settings
          </button>
        )}
        <div className="my-1 border-t border-border" />
        <button
          onClick={handleSignOut}
          className="flex w-full min-h-9 items-center gap-[9px] rounded-lg px-2.5 text-[12.5px] text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive"
        >
          <LogOut className="size-4 flex-shrink-0" />
          Sign out
        </button>
      </div>
      )}
    </aside>
    )
  }

  return (
    <>
      {sidebar}
      {showWorkflowPicker && (
        <WorkflowPickerOverlay
          workflows={manualWorkflows}
          onSelect={(workflow) => {
            onSelectWorkflow?.(workflow)
            setShowWorkflowPicker(false)
          }}
          onClose={() => setShowWorkflowPicker(false)}
        />
      )}
    </>
  )
})

/** WorkflowPickerOverlay is a keyboard-accessible overlay for picking a workflow.
 *  Closes on Escape key and click outside the card. */
function WorkflowPickerOverlay({
  workflows,
  onSelect,
  onClose,
}: {
  workflows: Workflow[]
  onSelect: (workflow: Workflow) => void
  onClose: () => void
}) {
  const handleKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose()
    },
    [onClose]
  )

  useEffect(() => {
    document.addEventListener("keydown", handleKeyDown)
    return () => document.removeEventListener("keydown", handleKeyDown)
  }, [handleKeyDown])

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="bg-card border border-border rounded-lg shadow-lg w-[360px] max-w-[95vw] max-h-[90vh] overflow-y-auto">
        <div className="flex items-center justify-between p-3 border-b border-border">
          <h3 className="text-sm font-medium">Select Workflow</h3>
          <Button variant="ghost" size="icon" className="size-7" onClick={onClose}>
            <X className="size-4" />
          </Button>
        </div>
        <div className="p-2 space-y-1">
          {workflows.length === 0 ? (
            <div className="px-3 py-4 text-sm text-muted-foreground">
              No manual workflows are available.
            </div>
          ) : workflows.map((workflow) => (
            <button
              key={`${workflow.workspaceName}/${workflow.name}`}
              className="w-full text-left px-3 py-2 text-sm rounded hover:bg-accent transition-colors"
              onClick={() => onSelect(workflow)}
            >
              <div className="font-medium">
                <WorkflowName name={workflow.name} />
              </div>
              <div className="text-xs text-muted-foreground truncate" title={workflow.workspaceName}>
                {workflow.workspaceName}
              </div>
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}
