"use client"

import { useState, useRef, useEffect } from "react"
import { cn } from "@/lib/utils"
import type { Claw, ClawStatus } from "@/lib/types"
import { COLOR_CLASSES } from "@/lib/mappers"
import { TagEditor } from "@/components/tag-editor"
import { patchClaw } from "@/lib/api"
import { Loader2, Pin, AlertCircle, Pencil } from "lucide-react"

interface ClawCardProps {
  claw: Claw
  isSelected: boolean
  onClick: () => void
  onTogglePin: (e: React.MouseEvent) => void
  onTagsChange?: (tags: string[]) => void
  showPinButton?: boolean
}

function formatUptime(seconds: number): string {
  if (seconds === 0) return "—"
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  const hours = Math.floor(seconds / 3600)
  const mins = Math.floor((seconds % 3600) / 60)
  return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`
}

function StatusIndicator({ status, isStreaming }: { status: ClawStatus; isStreaming: boolean }) {
  if (isStreaming) {
    return <Loader2 className="size-3.5 text-green-500 animate-spin shrink-0" />
  }
  if (status === "provisioning") {
    return <Loader2 className="size-3.5 text-blue-400 animate-spin shrink-0" />
  }
  if (status === "error") {
    return <AlertCircle className="size-3.5 text-red-500 shrink-0" />
  }
  return (
    <span
      className={cn(
        "size-2 rounded-full shrink-0",
        status === "connected" && "bg-green-500",
        status === "idle" && "bg-amber-500",
        status === "offline" && "bg-muted-foreground"
      )}
    />
  )
}

function UnreadBadge({ count }: { count: number }) {
  if (count === 0) return null
  
  return (
    <span className="flex items-center justify-center min-w-5 h-5 px-1.5 text-xs font-medium bg-blue-600 text-white rounded-full">
      {count > 99 ? "99+" : count}
    </span>
  )
}

export function ClawCard({ claw, isSelected, onClick, onTogglePin, onTagsChange, showPinButton = true }: ClawCardProps) {
  const [localTags, setLocalTags] = useState(claw.tags)
  const [localName, setLocalName] = useState(claw.name)
  const [editingName, setEditingName] = useState(false)
  const [nameValue, setNameValue] = useState(claw.name)
  const nameInputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (editingName) nameInputRef.current?.focus()
  }, [editingName])

  function handleTagsChange(tags: string[]) {
    setLocalTags(tags)
    onTagsChange?.(tags)
  }

  function startRename(e: React.MouseEvent) {
    e.stopPropagation()
    setNameValue(localName)
    setEditingName(true)
  }

  async function commitRename() {
    const name = nameValue.trim()
    setEditingName(false)
    if (!name || name === localName) return
    setLocalName(name)
    try {
      await patchClaw(claw.id, { name })
    } catch (e) {
      console.error("Failed to rename", e)
      setLocalName(claw.name)
    }
  }

  function handleNameKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Enter") commitRename()
    if (e.key === "Escape") { setEditingName(false); setNameValue(localName) }
  }
  const hasUnread = claw.unreadCount > 0
  const isPending = claw.status === "provisioning" || claw.status === "error"

  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault()
          onClick()
        }
      }}
      className={cn(
        "w-full text-left p-3 rounded-md transition-colors relative group border-l-2",
        COLOR_CLASSES[claw.color]?.border ?? "border-l-border",
        isPending ? "cursor-pointer opacity-70 hover:bg-accent" : "cursor-pointer hover:bg-accent",
        isSelected && "bg-accent",
        hasUnread && !isSelected && "bg-blue-950/30"
      )}
    >
      <div className="flex items-center gap-2 mb-1">
        <StatusIndicator status={claw.status} isStreaming={claw.isStreaming} />
        <span 
          className={cn(
            "font-mono text-sm truncate flex-1 group/name flex items-center gap-1",
            hasUnread ? "text-foreground font-medium" : "text-foreground"
          )}
        >
          {editingName ? (
            <input
              ref={nameInputRef}
              value={nameValue}
              onChange={(e) => setNameValue(e.target.value)}
              onKeyDown={handleNameKeyDown}
              onBlur={commitRename}
              onClick={(e) => e.stopPropagation()}
              className="bg-transparent border-b border-ring outline-none text-sm font-mono w-full"
            />
          ) : (
            <>
              <span className="truncate">{localName}</span>
              <button
                onClick={startRename}
                className="opacity-0 group-hover/name:opacity-60 hover:!opacity-100 transition-opacity flex-shrink-0"
                title="Rename"
              >
                <Pencil className="size-3" />
              </button>
            </>
          )}
        </span>
        <UnreadBadge count={claw.unreadCount} />
        {showPinButton && (
          <button
            onClick={onTogglePin}
            className={cn(
              "p-1 rounded hover:bg-background/50 transition-opacity",
              claw.pinned ? "opacity-100" : "opacity-0 group-hover:opacity-100"
            )}
            title={claw.pinned ? "Unpin" : "Pin"}
          >
            <Pin 
              className={cn(
                "size-3.5",
                claw.pinned ? "text-foreground fill-foreground" : "text-muted-foreground"
              )} 
            />
          </button>
        )}
      </div>
      <div className="flex items-center justify-between pl-5">
        <span className="text-xs text-muted-foreground truncate">
          {claw.template}
        </span>
        <span className="text-xs font-mono">
          {claw.status === "provisioning" ? (
            <span className="text-blue-400">starting...</span>
          ) : claw.status === "error" ? (
            <span className="text-red-500">error</span>
          ) : (
            <span className="text-muted-foreground">{formatUptime(claw.uptime)}</span>
          )}
        </span>
      </div>
      {(localTags.length > 0 || isSelected) && (
        <div className="mt-1.5 pl-5" onClick={(e) => e.stopPropagation()}>
          <TagEditor
            clawId={claw.id}
            tags={localTags}
            onTagsChange={handleTagsChange}
          />
        </div>
      )}
      {claw.isStreaming && (
        <div className="absolute left-0 top-0 bottom-0 w-0.5 bg-green-500 rounded-full" />
      )}
    </div>
  )
}
