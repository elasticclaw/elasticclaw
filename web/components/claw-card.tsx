"use client"

import { useState, useRef, useEffect } from "react"
import { createPortal } from "react-dom"
import { cn } from "@/lib/utils"
import type { Claw, ClawStatus } from "@/lib/types"
import { COLOR_CLASSES, CLAW_COLORS } from "@/lib/mappers"
import { TagEditor } from "@/components/tag-editor"
import { patchClaw } from "@/lib/api"
import { Loader2, Pin, AlertCircle, Pencil } from "lucide-react"
import { BootstrapProgress } from "@/components/bootstrap-progress"
import { ClawTitle } from "@/components/claw-title"

interface ClawCardProps {
  claw: Claw
  isSelected: boolean
  onClick: () => void
  onTogglePin: (e: React.MouseEvent) => void
  onTagsChange?: (tags: string[]) => void
  showPinButton?: boolean
  /** One-line "what it is running right now" — shown under the name when set. */
  activityLine?: string
  stateChip?: React.ReactNode
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
      className="size-2 rounded-full shrink-0"
      style={{
        backgroundColor:
          status === "connected" ? "var(--status-connected)"
          : status === "idle" ? "var(--status-idle)"
          : "var(--status-offline)",
      }}
    />
  )
}

/* Freshness for the status line: uptime while the agent is up, else how long
   since the hub last saw it. */
function rowAge(claw: Claw) {
  if ((claw.status === "connected" || claw.isStreaming) && claw.uptime > 0) {
    const minutes = Math.floor(claw.uptime / 60)
    if (minutes < 60) return `${minutes}m`
    return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, "0")}m`
  }
  if (!claw.last_seen) return undefined
  const ms = Date.now() - new Date(claw.last_seen).getTime()
  if (Number.isNaN(ms) || ms < 0) return undefined
  const minutes = Math.floor(ms / 60_000)
  if (minutes < 1) return "now"
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  return `${Math.floor(hours / 24)}d ago`
}

function UnreadBadge({ count }: { count: number }) {
  if (count === 0) return null
  
  return (
    <span className="flex items-center justify-center min-w-5 h-5 px-1.5 text-xs font-medium bg-blue-600 text-white rounded-full">
      {count > 99 ? "99+" : count}
    </span>
  )
}

export function ClawCard({ claw, isSelected, onClick, onTogglePin, onTagsChange, showPinButton = true, activityLine, stateChip }: ClawCardProps) {
  const [localTags, setLocalTags] = useState(claw.tags)
  const [localName, setLocalName] = useState(claw.name)
  const [localColor, setLocalColor] = useState(claw.color)
  const [showColorPicker, setShowColorPicker] = useState(false)
  const [pickerPos, setPickerPos] = useState({ top: 0, left: 0 })
  const colorDotRef = useRef<HTMLButtonElement>(null)
  const [editingName, setEditingName] = useState(false)
  const [nameValue, setNameValue] = useState(claw.name)
  const nameInputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (editingName) nameInputRef.current?.focus()
  }, [editingName])

  useEffect(() => {
    if (!showColorPicker) return
    const handler = (e: MouseEvent) => {
      const target = e.target as HTMLElement
      if (!target.closest('[data-color-picker]')) setShowColorPicker(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [showColorPicker])

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

  async function handleColorChange(color: string) {
    setLocalColor(color)
    setShowColorPicker(false)
    try {
      await patchClaw(claw.id, { color })
    } catch (e) {
      console.error("Failed to update color", e)
      setLocalColor(claw.color)
    }
  }
  const hasUnread = claw.unreadCount > 0
  const railClass = claw.isStreaming
    ? "border-l-green-500"
    : claw.status === "provisioning"
      ? "border-l-blue-400"
      : claw.status === "error"
        ? "border-l-red-500"
        : COLOR_CLASSES[localColor]?.border ?? "border-l-border"

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
        "w-full min-w-0 text-left p-3 rounded-md transition-colors relative group border-l-2 cursor-pointer",
        railClass,
        // Hover stays translucent so the opaque selected surface reads apart
        // from it; only offline rows dim — provisioning and error need eyes.
        "hover:bg-muted/45",
        claw.status === "offline" && "opacity-75",
        isSelected && "bg-accent",
        hasUnread && !isSelected && "bg-blue-950/30"
      )}
    >
      <div className="flex items-center gap-2 mb-1">
        <StatusIndicator status={claw.status} isStreaming={claw.isStreaming} />
        <span 
          className={cn(
            "font-mono text-sm min-w-0 flex-1 group/name flex items-center gap-1",
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
              <ClawTitle
                name={localName}
                githubIssueId={claw.githubIssueId}
                githubIssueUrl={claw.githubIssueUrl}
                className="flex-1"
              />
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
      {/* Status line, one baseline row like the kit AgentRow: STATE chip, a
          detail that is never empty (live activity, else the template), and
          the age on the right edge. */}
      <div className="mt-[3px] flex min-w-0 items-baseline gap-1.5 pl-5 pr-1">
        {stateChip}
        {(activityLine || claw.template) && (
          <span
            className={cn("min-w-0 flex-1 truncate font-mono text-[11px] leading-4", claw.status === "error" ? "text-[var(--text-error,var(--destructive))]" : "text-muted-foreground")}
            title={activityLine || claw.template}
          >
            {activityLine || claw.template}
          </span>
        )}
        {rowAge(claw) && <span className="ml-auto shrink-0 font-mono text-[10px] text-muted-foreground">{rowAge(claw)}</span>}
      </div>
      <div className="min-w-0 max-w-[170px] overflow-hidden pl-5 pr-1">
        <BootstrapProgress claw={claw} variant="sidebar" />
      </div>
      {(localTags.length > 0 || isSelected) && (
        <div className="mt-1.5 max-w-[170px] overflow-hidden pl-5 pr-1 flex items-start gap-2" onClick={(e) => e.stopPropagation()}>
          <TagEditor
            clawId={claw.id}
            tags={localTags}
            onTagsChange={handleTagsChange}
          />
          {/* Color picker */}
          <div className="relative flex-shrink-0 ml-auto" data-color-picker>
            <button
              ref={colorDotRef}
              onClick={() => {
                if (!showColorPicker && colorDotRef.current) {
                  const rect = colorDotRef.current.getBoundingClientRect()
                  setPickerPos({ top: rect.bottom + 6, left: rect.left })
                }
                setShowColorPicker((v) => !v)
              }}
              className={cn(
                "size-3 rounded-full mt-0.5 ring-2 ring-offset-1 ring-offset-background transition-all",
                COLOR_CLASSES[localColor]?.dot ?? "bg-muted",
                showColorPicker ? "ring-foreground" : "ring-transparent hover:ring-muted-foreground"
              )}
              title="Change color"
            />
            {showColorPicker && typeof document !== 'undefined' && createPortal(
              <div
                data-color-picker
                className="fixed bg-popover border border-border rounded-lg p-2 shadow-xl"
                style={{ top: pickerPos.top, left: pickerPos.left, zIndex: 99999 }}
              >
                <div className="grid grid-cols-8 gap-1">
                  {CLAW_COLORS.map((c) => (
                    <button
                      key={c}
                      onClick={() => handleColorChange(c)}
                      className={cn(
                        "size-4 rounded-full transition-transform hover:scale-125",
                        COLOR_CLASSES[c]?.dot,
                        localColor === c && "ring-2 ring-offset-1 ring-offset-background ring-foreground"
                      )}
                      title={c}
                    />
                  ))}
                </div>
              </div>,
              document.body
            )}
          </div>
        </div>
      )}
    </div>
  )
}
