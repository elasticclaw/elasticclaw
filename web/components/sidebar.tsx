"use client"

import { Search, Pin, X, ChevronDown, PanelLeftClose, PanelLeft, Loader2, AlertCircle, LogOut, Settings } from "lucide-react"
import { useBranding } from "@/hooks/use-branding"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { ScrollArea } from "@/components/ui/scroll-area"
import { ClawCard } from "@/components/claw-card"
import { clearConfig } from "@/lib/api"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { cn } from "@/lib/utils"
import type { Claw } from "@/lib/types"
import {
  DndContext,
  closestCenter,
  PointerSensor,
  useSensor,
  useSensors,
  DragEndEvent,
  DragOverlay,
  DragStartEvent,
} from "@dnd-kit/core"
import {
  SortableContext,
  useSortable,
  verticalListSortingStrategy,
  arrayMove,
} from "@dnd-kit/sortable"
import { CSS } from "@dnd-kit/utilities"
import { useState } from "react"

type TagFilter = string

interface SidebarProps {
  claws: Claw[]
  pinnedClaws: Claw[]
  allClawIds: string[]
  selectedClawId: string | null
  onSelectClaw: (id: string) => void
  onTogglePin: (id: string) => void
  onReorderClaws: (ids: string[]) => void
  onSpawn: () => void
  searchQuery: string
  onSearchChange: (query: string) => void
  allTags: string[]
  activeTagFilters: TagFilter[]
  onAddTagFilter: (filter: TagFilter) => void
  onRemoveTagFilter: (filter: TagFilter) => void
  onClearTagFilters: () => void
  isCollapsed: boolean
  onToggleCollapse: () => void
  isAdmin?: boolean
}

/** Thin wrapper that gives ClawCard sortable DnD powers */
function SortableClawCard({
  claw,
  isSelected,
  onClick,
  onTogglePin,
}: {
  claw: Claw
  isSelected: boolean
  onClick: () => void
  onTogglePin: (e: React.MouseEvent) => void
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } =
    useSortable({ id: claw.id })

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.4 : 1,
    cursor: isDragging ? "grabbing" : undefined,
  }

  return (
    <div ref={setNodeRef} style={style} {...attributes} {...listeners}>
      <ClawCard
        claw={claw}
        isSelected={isSelected}
        onClick={onClick}
        onTogglePin={onTogglePin}
      />
    </div>
  )
}

export function Sidebar({
  claws,
  pinnedClaws,
  allClawIds,
  selectedClawId,
  onSelectClaw,
  onTogglePin,
  onReorderClaws,
  onSpawn,
  searchQuery,
  onSearchChange,
  allTags,
  activeTagFilters,
  onAddTagFilter,
  onRemoveTagFilter,
  onClearTagFilters,
  isCollapsed,
  onToggleCollapse,
  isAdmin = true,
}: SidebarProps) {
  const tagKeys = allTags
  const { appName } = useBranding()
  const [activeDragClaw, setActiveDragClaw] = useState<Claw | null>(null)

  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: {
        // Require 6px movement before drag starts — lets clicks pass through
        distance: 6,
      },
    })
  )

  function handleDragStart(event: DragStartEvent) {
    const allVisible = [...pinnedClaws, ...claws]
    const found = allVisible.find((c) => c.id === event.active.id)
    setActiveDragClaw(found ?? null)
  }

  function handleDragEnd(event: DragEndEvent) {
    setActiveDragClaw(null)
    const { active, over } = event
    if (!over || active.id === over.id) return

    // Figure out which list the drag happened in (pinned vs main)
    const inPinned = pinnedClaws.some((c) => c.id === active.id)
    const targetInPinned = pinnedClaws.some((c) => c.id === over.id)

    if (inPinned !== targetInPinned) return // don't allow crossing sections

    if (inPinned) {
      // Reorder within pinned — update full order accounting for the pinned move
      const pinnedIds = pinnedClaws.map((c) => c.id)
      const oldIdx = pinnedIds.indexOf(active.id as string)
      const newIdx = pinnedIds.indexOf(over.id as string)
      const reordered = arrayMove(pinnedIds, oldIdx, newIdx)
      const unpinnedIds = allClawIds.filter((id) => !pinnedIds.includes(id))
      onReorderClaws([...reordered, ...unpinnedIds])
    } else {
      const unpinnedIds = claws.map((c) => c.id)
      const oldIdx = unpinnedIds.indexOf(active.id as string)
      const newIdx = unpinnedIds.indexOf(over.id as string)
      const reordered = arrayMove(unpinnedIds, oldIdx, newIdx)
      const pinnedIds = pinnedClaws.map((c) => c.id)
      onReorderClaws([...pinnedIds, ...reordered])
    }
  }

  // Merge pinned + unpinned for collapsed view (order already applied by parent)
  const allClaws = [...pinnedClaws, ...claws.filter(c => !pinnedClaws.find(p => p.id === c.id))]

  if (isCollapsed) {
    return (
      <aside className="w-12 h-screen flex flex-col border-r border-border bg-card">
        <div className="p-2 border-b border-border">
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
        <div className="flex flex-col items-center gap-1 py-2 overflow-y-auto flex-1">
          {allClaws.map((claw) => {
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
                  <Loader2 className="size-3.5 text-blue-400 animate-spin" />
                ) : claw.status === "error" ? (
                  <AlertCircle className="size-3.5 text-red-500" />
                ) : claw.isStreaming ? (
                  <Loader2 className="size-3.5 text-green-500 animate-spin" />
                ) : (
                  <span className={cn(
                    "size-2.5 rounded-full",
                    claw.status === "connected" && "bg-green-500",
                    claw.status === "idle" && "bg-amber-500",
                    claw.status === "offline" && "bg-muted-foreground/50"
                  )} />
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
      </aside>
    )
  }
  
  return (
    <aside className="w-[260px] h-screen flex flex-col border-r border-border bg-card">
      <div className="flex items-center justify-between p-4 border-b border-border">
        <h1 className="text-lg font-semibold tracking-tight text-foreground">
          {appName}
        </h1>
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon"
            onClick={onToggleCollapse}
            className="size-8"
            title="Collapse sidebar"
          >
            <PanelLeftClose className="size-4" />
          </Button>
        </div>
      </div>

      <div className="p-3 border-b border-border space-y-2">
        <div className="relative">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 size-4 text-muted-foreground" />
          <Input
            placeholder="Filter by name or template..."
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
        collisionDetection={closestCenter}
        onDragStart={handleDragStart}
        onDragEnd={handleDragEnd}
      >
        {/* Pinned Section */}
        {pinnedClaws.length > 0 && !searchQuery && (
          <div className="border-b border-border">
            <div className="flex items-center gap-1.5 px-4 py-2">
              <Pin className="size-3 text-muted-foreground" />
              <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">
                Pinned
              </span>
            </div>
            <div className="px-2 pb-2">
              <SortableContext
                items={pinnedClaws.map((c) => c.id)}
                strategy={verticalListSortingStrategy}
              >
                {pinnedClaws.map((claw) => (
                  <SortableClawCard
                    key={claw.id}
                    claw={claw}
                    isSelected={claw.id === selectedClawId}
                    onClick={() => onSelectClaw(claw.id)}
                    onTogglePin={(e) => {
                      e.stopPropagation()
                      onTogglePin(claw.id)
                    }}
                  />
                ))}
              </SortableContext>
            </div>
          </div>
        )}

        <ScrollArea className="flex-1 scrollbar-hide">
          <div className="p-2">
            {!searchQuery && pinnedClaws.length > 0 && claws.length > 0 && (
              <div className="flex items-center gap-1.5 px-2 py-2">
                <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">
                  All Claws
                </span>
              </div>
            )}
            {claws.length === 0 && pinnedClaws.length === 0 ? (
              <p className="text-sm text-muted-foreground text-center py-8">
                No claws found
              </p>
            ) : (
              <SortableContext
                items={claws.map((c) => c.id)}
                strategy={verticalListSortingStrategy}
              >
                {claws.map((claw) => (
                  <SortableClawCard
                    key={claw.id}
                    claw={claw}
                    isSelected={claw.id === selectedClawId}
                    onClick={() => onSelectClaw(claw.id)}
                    onTogglePin={(e) => {
                      e.stopPropagation()
                      onTogglePin(claw.id)
                    }}
                  />
                ))}
              </SortableContext>
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

      {/* Logout + Settings */}
      <div className="p-2 border-t border-border">
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            className="flex-1 justify-start gap-2 text-muted-foreground hover:text-foreground"
            onClick={async () => {
              const token = sessionStorage.getItem("ec_github_token") || sessionStorage.getItem("ec_hub_token") || ""
              if (token) {
                const { getHubUrl } = await import("@/lib/hub-url")
                const hubUrl = getHubUrl()
                const logoutUrl = hubUrl ? `${hubUrl}/api/auth/logout` : "/api/auth/logout"
                await fetch(logoutUrl, { method: "POST", headers: { Authorization: `Bearer ${token}` } }).catch(() => {})
              }
              clearConfig()
              window.location.href = "/login"
            }}
          >
            <LogOut className="size-4" />
            Sign out
          </Button>
          {isAdmin && (
            <Button
              variant="ghost"
              size="icon"
              className="size-8 text-muted-foreground hover:text-foreground flex-shrink-0"
              onClick={() => { window.location.href = "/settings" }}
              title="Settings"
            >
              <Settings className="size-4" />
            </Button>
          )}
        </div>
      </div>
    </aside>
  )
}
