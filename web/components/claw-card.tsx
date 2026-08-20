"use client"

import { cn } from "@/lib/utils"
import type { Claw, ClawStatus } from "@/lib/types"
import { COLOR_CLASSES } from "@/lib/mappers"
import { Loader2, Pin, AlertCircle } from "lucide-react"
import { BootstrapProgress } from "@/components/bootstrap-progress"
import { ClawTitle } from "@/components/claw-title"
import { useNowMinuteTick } from "@/hooks/use-now"
import { memo } from "react"

interface ClawCardProps {
  claw: Claw
  isSelected: boolean
  onClick: () => void
  onTogglePin: (e: React.MouseEvent) => void
  showPinButton?: boolean
  /** One-line "what it is running right now" — shown under the name when set. */
  activityLine?: string
  stateChip?: React.ReactNode
}

function StatusIndicator({ status, isStreaming }: { status: ClawStatus; isStreaming: boolean }) {
  if (isStreaming) {
    return <Loader2 className="size-3.5 animate-spin shrink-0" style={{ color: "var(--status-streaming)" }} />
  }
  if (status === "provisioning") {
    return <Loader2 className="size-3.5 animate-spin shrink-0" style={{ color: "var(--status-provisioning)" }} />
  }
  if (status === "error") {
    return <AlertCircle className="size-3.5 shrink-0" style={{ color: "var(--status-error)" }} />
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

function UnreadBadge({ count }: { count: number }) {
  if (count === 0) return null

  return (
    <span className="flex items-center justify-center min-w-5 h-5 px-1.5 text-xs font-medium bg-blue-600 text-white rounded-full">
      {count > 99 ? "99+" : count}
    </span>
  )
}

/* Freshness for the status line: uptime while the agent is up, else how long
   since the hub last saw it. */
function rowAge(claw: Claw, now: number) {
  if ((claw.status === "connected" || claw.isStreaming) && claw.uptime > 0) {
    const minutes = Math.floor(claw.uptime / 60)
    if (minutes < 60) return `${minutes}m`
    return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, "0")}m`
  }
  if (!claw.last_seen) return undefined
  const ms = now - new Date(claw.last_seen).getTime()
  if (Number.isNaN(ms) || ms < 0) return undefined
  const minutes = Math.floor(ms / 60_000)
  if (minutes < 1) return "now"
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  return `${Math.floor(hours / 24)}d ago`
}

/* The sidebar list row, kept to the kit AgentRow anatomy: rail, status glyph,
   mono name, unread pill, hover-only pin; then one status line (state chip +
   detail + age) and up to three read-only tags. Renaming, tag editing and the
   color picker live in the board card's Agent details sheet. */
export const ClawCard = memo(function ClawCard({ claw, isSelected, onClick, onTogglePin, showPinButton = true, activityLine, stateChip }: ClawCardProps) {
  const now = useNowMinuteTick(Boolean(claw.last_seen))
  const hasUnread = claw.unreadCount > 0
  const railStyle = claw.isStreaming
    ? { borderLeftColor: "var(--status-streaming)" }
    : claw.status === "provisioning"
      ? { borderLeftColor: "var(--status-provisioning)" }
      : claw.status === "error"
        ? { borderLeftColor: "var(--status-error)" }
        : undefined
  const railClass = railStyle ? "" : COLOR_CLASSES[claw.color]?.border ?? "border-l-border"
  const detail = activityLine || claw.template
  const age = rowAge(claw, now)

  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.target !== e.currentTarget) return
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
      style={railStyle}
    >
      <div className="flex items-center gap-2">
        <StatusIndicator status={claw.status} isStreaming={claw.isStreaming} />
        <span
          className={cn(
            "font-mono text-sm min-w-0 flex-1",
            hasUnread || isSelected ? "text-foreground font-medium" : "text-foreground"
          )}
        >
          <ClawTitle
            name={claw.name}
            githubIssueId={claw.githubIssueId}
            githubIssueUrl={claw.githubIssueUrl}
          />
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
        {detail && (
          <span
            className={cn("min-w-0 flex-1 truncate font-mono text-[11px] leading-4", claw.status === "error" ? "text-[var(--text-error)]" : "text-muted-foreground")}
            title={detail}
          >
            {detail}
          </span>
        )}
        {age && <span className="ml-auto shrink-0 font-mono text-[10px] text-muted-foreground">{age}</span>}
      </div>
      <div className="min-w-0 max-w-[170px] overflow-hidden pl-5 pr-1">
        <BootstrapProgress claw={claw} variant="sidebar" />
      </div>
      {claw.tags.length > 0 && (
        <div className="mt-1.5 flex max-w-[170px] flex-wrap items-center gap-1 overflow-hidden pl-5 pr-1">
          {claw.tags.slice(0, 3).map((tag) => (
            <span key={tag} title={tag} className="max-w-[120px] truncate rounded-sm bg-secondary px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
              {tag}
            </span>
          ))}
          {claw.tags.length > 3 && <span className="text-[11px] text-muted-foreground">+{claw.tags.length - 3} more</span>}
        </div>
      )}
    </div>
  )
})
