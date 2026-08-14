"use client"

import { useState, useRef, useEffect, useCallback, useMemo, memo } from "react"
import { Send, Terminal, TerminalSquare, ChevronLeft, ChevronRight, ChevronDown, Loader2, LayoutGrid, Info, MessageSquare, Trash2, AlertCircle, Wrench, GripVertical, Settings2, Paperclip, File as FileIcon, X, Menu, MoreVertical, LogOut, ClipboardCopy } from "lucide-react"
import {
  compactActivityRuns,
  demoteStaleRunning,
  groupIntoTurns,
  latestRunningStep,
  pairActivitySteps,
  timelineStats,
  trailingActivityRun,
} from "@/lib/turns"
import { AgentTimeline } from "@/components/agent-timeline/timeline"
import { TimelineToolbar, useTimelineDensity } from "@/components/agent-timeline/timeline-toolbar"
import { NowStrip } from "@/components/agent-timeline/now-strip"
import { StepRow } from "@/components/agent-timeline/step-row"
import { ActivitySummaryBlock } from "@/components/agent-timeline/activity-summary-block"
import { useNowTick } from "@/hooks/use-now"
import { CopyTranscriptButton } from "@/components/copy-transcript-button"
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
  horizontalListSortingStrategy,
  arrayMove,
} from "@dnd-kit/sortable"
import { CSS } from "@dnd-kit/utilities"
import { MarkdownContent } from "@/components/markdown-content"
import { COLOR_CLASSES } from "@/lib/mappers"
import { useWindowedMessages } from "@/hooks/use-windowed-messages"
import { useProgrammaticScrollFlag, usePinnedAutoScroll } from "@/hooks/use-pinned-scroll"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter } from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useIsMobile } from "@/hooks/use-mobile"
import { signOut } from "@/lib/sign-out"
import { copyTextToClipboard, formatChatTranscript } from "@/lib/transcript"
import { cn } from "@/lib/utils"
import type { Claw, DependencyStatus, Message, ClawStatus } from "@/lib/types"
import { getTerminalWsUrl, fetchClawPRs, type ClawPR } from "@/lib/api"
import { buildAttachmentsFooter, splitAttachmentsFooter, formatBytes, type ParsedAttachment } from "@/lib/attachments"
import { useAttachments } from "@/hooks/use-attachments"
import { AttachmentChip } from "@/components/attachment-chip"
import dynamic from "next/dynamic"
import { useBranding } from "@/hooks/use-branding"
import { BootstrapProgress } from "@/components/bootstrap-progress"
import { ClawTitle } from "@/components/claw-title"
import { windowMessagesByDurableCount } from "@/lib/messages"
import { DependencyDowntimeBanner } from "@/components/dependency-downtime-banner"
import type { TypewriterState } from "@/hooks/use-typewriter"

const XTerminal = dynamic(
  () => import("@/components/terminal").then((m) => m.XTerminal),
  { ssr: false }
)

interface ConversationViewProps {
  loading?: boolean
  hubError?: string | null
  claw: Claw | null
  allClaws: Claw[]
  downtimeDependencies: DependencyStatus[]
  messages: Message[]
  allMessages: Record<string, Message[]>
  streamingBuffers: Record<string, TypewriterState>
  onSendMessage: (content: string) => void
  onSendMessageToClaw: (clawId: string, content: string) => void
  onKill: () => void
  onKillClaw: (clawId: string) => void
  onSelectClaw: (id: string) => void
  onDeselectClaw: () => void
  onReorderClaws: (ids: string[]) => void
  /** Mobile only: opens the sidebar drawer from the board header hamburger. */
  onOpenMenu?: () => void
}

const FOLLOW_LATEST_THRESHOLD_PX = 24
/** Last N durable conversation turns on board cards (activities do not count). */
const BOARD_CARD_DURABLE_MESSAGE_WINDOW = 50
const EMPTY_MESSAGES: Message[] = []
const noopClawAction = (_clawId: string) => {}
const noopClawMessageAction = (_clawId: string, _content: string) => {}

// The typewriter reveals text every animation frame, but re-parsing markdown that
// often is what made the board expensive. Sample the buffer instead: the reveal
// still looks continuous at 8Hz, and the parse rate stays bounded.
const STREAM_MARKDOWN_INTERVAL_MS = 125

function useThrottledText(text: string): string {
  const [sampled, setSampled] = useState(text)
  const latest = useRef(text)

  useEffect(() => {
    latest.current = text
  }, [text])

  useEffect(() => {
    const timer = window.setInterval(() => {
      setSampled((prev) => (prev === latest.current ? prev : latest.current))
    }, STREAM_MARKDOWN_INTERVAL_MS)
    return () => window.clearInterval(timer)
  }, [])

  // Never lag behind a shrinking buffer (split/clear resets it to "").
  return text.length < sampled.length ? text : sampled
}

// Renders the live typewriter buffer for one claw. Kept as its own component so the
// rAF tick only re-renders this subtree instead of the whole card/chat message list.
// Styling mirrors the card message row ("card") and MessageBubble ("chat") so the
// hand-off to the finalized message is invisible.
function StreamingMessage({
  state,
  variant,
  clawName,
  clawColor,
}: {
  state: TypewriterState
  variant: "card" | "chat"
  clawName: string
  clawColor?: string
}) {
  // Streaming messages have no server timestamp yet; freeze the start time so the
  // header does not change while the typewriter drains.
  const [startedAt] = useState(() => new Date())
  const text = useThrottledText(state.text)

  if (!state.hadChunks) {
    return variant === "card" ? (
      <div className="flex gap-1 py-2 pl-2">
        <span className="size-1.5 rounded-full bg-muted-foreground/50 animate-bounce [animation-delay:0ms]" />
        <span className="size-1.5 rounded-full bg-muted-foreground/50 animate-bounce [animation-delay:150ms]" />
        <span className="size-1.5 rounded-full bg-muted-foreground/50 animate-bounce [animation-delay:300ms]" />
      </div>
    ) : (
      <div className="flex justify-start">
        <div className="bg-secondary rounded-lg px-4 py-3">
          <div className="flex items-center gap-1.5">
            <span className="size-1.5 rounded-full bg-muted-foreground/60 animate-bounce [animation-delay:0ms]" />
            <span className="size-1.5 rounded-full bg-muted-foreground/60 animate-bounce [animation-delay:150ms]" />
            <span className="size-1.5 rounded-full bg-muted-foreground/60 animate-bounce [animation-delay:300ms]" />
          </div>
        </div>
      </div>
    )
  }

  if (!state.text) return null

  if (variant === "card") {
    return (
      <div className="text-xs p-2 rounded bg-secondary mr-4">
        <div className="flex items-center gap-1 mb-0.5">
          <span className="font-medium text-foreground/70">{clawName}</span>
          <span className="text-muted-foreground" suppressHydrationWarning>
            {formatTimestamp(startedAt)}
          </span>
        </div>
        <MarkdownContent content={text} className="text-xs text-foreground" />
      </div>
    )
  }

  return (
    <div className="flex w-full justify-start">
      <div
        className={cn(
          "w-fit max-w-[88%] md:w-[70%] md:max-w-none min-w-0 rounded-lg px-4 py-3",
          (clawColor && COLOR_CLASSES[clawColor]?.bubble) || "bg-secondary"
        )}
      >
        <div className="flex items-center gap-2 mb-1">
          <span className="text-xs font-medium text-foreground">{clawName}</span>
          <span className="text-xs text-muted-foreground" suppressHydrationWarning>
            {formatTimestamp(startedAt)}
          </span>
        </div>
        <MarkdownContent content={text} className="text-sm text-foreground" />
      </div>
    </div>
  )
}

function formatUptime(seconds: number): string {
  if (seconds === 0) return "—"
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  const hours = Math.floor(seconds / 3600)
  const mins = Math.floor((seconds % 3600) / 60)
  return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`
}

function StatusBadge({ status }: { status: ClawStatus }) {
  return (
    <Badge
      variant="outline"
      className={cn(
        "text-xs font-medium",
        status === "connected" && "border-green-500/50 text-green-500",
        status === "idle" && "border-amber-500/50 text-amber-500",
        status === "offline" && "border-red-500/50 text-red-500"
      )}
    >
      {status}
    </Badge>
  )
}

function StatusDot({ status, isStreaming }: { status: ClawStatus; isStreaming: boolean }) {
  if (isStreaming) return <Loader2 className="size-3.5 text-green-500 animate-spin" />
  if (status === "provisioning") return <Loader2 className="size-3.5 text-blue-400 animate-spin" />
  if (status === "error") return <AlertCircle className="size-3.5 text-red-500" />
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

function ContextProgressBar({ usage, size = "sm" }: { usage: number; size?: "sm" | "lg" }) {
  const getColor = (value: number) => {
    if (value >= 90) return "bg-red-500"
    if (value >= 70) return "bg-amber-500"
    return "bg-green-500"
  }

  const getBgColor = (value: number) => {
    if (value >= 90) return "bg-red-500/20"
    if (value >= 70) return "bg-amber-500/20"
    return "bg-green-500/20"
  }

  if (size === "lg") {
    return (
      <div className="group relative flex items-center">
        <div 
          className={cn(
            "h-1.5 group-hover:h-3 rounded-full transition-all duration-200 overflow-hidden",
            "w-24 group-hover:w-32",
            getBgColor(usage)
          )}
        >
          <div 
            className={cn("h-full rounded-full transition-all", getColor(usage))}
            style={{ width: `${usage}%` }}
          />
        </div>
        <span className="ml-2 text-xs text-muted-foreground opacity-0 group-hover:opacity-100 transition-opacity font-mono">
          {usage}%
        </span>
      </div>
    )
  }

  return (
    <div className="group relative">
      <div 
        className={cn(
          "h-1 group-hover:h-2.5 rounded-full transition-all duration-200 overflow-hidden w-full",
          getBgColor(usage)
        )}
      >
        <div 
          className={cn("h-full rounded-full transition-all", getColor(usage))}
          style={{ width: `${usage}%` }}
        />
      </div>
      <div className="absolute inset-0 flex items-center justify-center opacity-0 group-hover:opacity-100 transition-opacity">
        <span className="text-[9px] font-mono font-medium text-foreground drop-shadow-sm">
          {usage}%
        </span>
      </div>
    </div>
  )
}

function KillConfirmDialog({ clawName, open, onConfirm, onCancel }: {
  clawName: string
  open: boolean
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) onCancel() }}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle>Kill {clawName}?</DialogTitle>
          <DialogDescription>
            This will terminate the agent and destroy the VM. Any unsaved work will be lost.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter className="gap-2">
          <Button variant="outline" onClick={onCancel}>Cancel</Button>
          <Button variant="destructive" onClick={onConfirm}>Kill</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ClawCardBack({ claw }: { claw: Claw }) {
  const [prs, setPrs] = useState<ClawPR[]>([])

  useEffect(() => {
    fetchClawPRs(claw.id).then(setPrs).catch(() => {})
  }, [claw.id])

  return (
    <div className="flex-1 overflow-y-auto scrollbar-thin p-4 space-y-4">
      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
          Purpose
        </h3>
        <p className="text-sm text-foreground leading-relaxed">
          {claw.description || "No description provided for this agent."}
        </p>
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
          Source
        </h3>
        <p className="text-sm font-mono text-foreground">
          {claw.template}
        </p>
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
          Status
        </h3>
        <div className="flex items-center gap-2">
          <StatusDot status={claw.status} isStreaming={claw.isStreaming} />
          <span className="text-sm text-foreground capitalize">{claw.status}</span>
          {claw.isStreaming && (
            <span className="text-xs text-green-500">(streaming)</span>
          )}
        </div>
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
          Context Usage
        </h3>
        <div className="flex items-center gap-2">
          <div className="flex-1">
            <ContextProgressBar usage={claw.contextUsage} size="sm" />
          </div>
          <span className="text-sm font-mono text-foreground">{claw.contextUsage}%</span>
        </div>
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
          Uptime
        </h3>
        <p className="text-sm font-mono text-foreground">
          {formatUptime(claw.uptime)}
        </p>
      </div>

      {claw.tags.length > 0 && (
        <div>
          <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
            Tags
          </h3>
          <div className="flex flex-wrap gap-1.5">
            {claw.tags.map((tag) => (
              <span
                key={tag}
                className="inline-flex items-center px-2 py-1 text-xs font-medium bg-secondary text-muted-foreground rounded"
              >
                {tag}
              </span>
            ))}
          </div>
        </div>
      )}

      {prs.length > 0 && (
        <div>
          <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">Pull Requests</h3>
          <div className="space-y-1.5">
            {prs.map(pr => (
              <a key={pr.id} href={pr.url} target="_blank" rel="noopener noreferrer"
                className="flex items-center gap-2 text-xs text-blue-400 hover:underline">
                <span className="font-mono text-muted-foreground">#{pr.prNumber}</span>
                <span className="truncate">{pr.repo}</span>
              </a>
            ))}
          </div>
        </div>
      )}


    </div>
  )
}

const ClawBoardCard = memo(function ClawBoardCard({
  claw, 
  messages,
  streamingBuffer,
  onClick,
  onSendMessage,
  onKill,
  dragHandleProps,
}: { 
  claw: Claw
  messages: Message[]
  streamingBuffer?: TypewriterState
  onClick: (clawId: string) => void
  onSendMessage: (clawId: string, content: string) => void
  onKill: (clawId: string) => void
  dragHandleProps?: React.HTMLAttributes<HTMLElement>
}) {
  const [input, setInput] = useState("")
  const cardTextareaRef = useRef<HTMLTextAreaElement>(null)
  const cardFileInputRef = useRef<HTMLInputElement>(null)
  const [isFlipped, setIsFlipped] = useState(false)
  // The 3D flip (perspective/preserve-3d) is brittle on mobile Safari — on
  // phones the card is full-width in a vertical list and front/back is a
  // plain visibility swap instead of a rotateY transform.
  const isMobile = useIsMobile()
  const [showTerminal, setShowTerminal] = useState(false)
  const [confirmKill, setConfirmKill] = useState(false)
  const hasUnread = claw.unreadCount > 0
  const isPending = claw.status === "provisioning" || claw.status === "error" || claw.status === "offline"
  const msgScrollRef = useRef<HTMLDivElement>(null)
  const cardFollowingLatest = useRef(true)
  const [isCardFollowingLatest, setIsCardFollowingLatest] = useState(true)
  // Window by durable turns only — a tool-activity flood must not age out
  // earlier user/claw messages from the card (refresh would still show them).
  const visibleMessages = useMemo(
    () => windowMessagesByDurableCount(messages, BOARD_CARD_DURABLE_MESSAGE_WINDOW),
    [messages]
  )
  const conversationItems = useMemo(() => compactActivityRuns(visibleMessages), [visibleMessages])
  // Latest step of the trailing activity run — the card's "what is happening
  // right now" row (paired start/terminal, live elapsed while running).
  const latestStep = useMemo(() => {
    const steps = demoteStaleRunning(pairActivitySteps(trailingActivityRun(visibleMessages)), true)
    return steps.length > 0 ? steps[steps.length - 1] : null
  }, [visibleMessages])
  const activityNowMs = useNowTick(Boolean(latestStep))
  const isStreaming = claw.isStreaming || Boolean(streamingBuffer?.hadChunks && streamingBuffer.text)

  const {
    attachments,
    dragHover,
    addFiles,
    removeAttachment,
    clearAttachments,
    onDragEnter,
    onDragOver,
    onDragLeave,
    onDrop,
    onPaste,
  } = useAttachments(claw.id)
  const stillUploading = attachments.some((a) => a.status === "uploading")
  const hasErrored = attachments.some((a) => a.status === "error")
  const canSubmitCard = !isPending && !stillUploading && !hasErrored && (input.trim().length > 0 || attachments.some((a) => a.status === "ready"))

  const cardContentRef = useRef<HTMLDivElement>(null)
  const { isProgrammaticRef: isCardProgrammaticScrollRef, mark: markCardProgrammaticScroll } = useProgrammaticScrollFlag()

  // Follow new rows and late content settling (rich activity rows sizing,
  // streaming growth) while following; never touch the scroll otherwise.
  usePinnedAutoScroll({
    scrollRef: msgScrollRef,
    contentRef: cardContentRef,
    pinnedRef: cardFollowingLatest,
    markProgrammaticScroll: markCardProgrammaticScroll,
    bottomAnchor: messages,
  })

  const handleCardScroll = useCallback(() => {
    // Scrolls we initiate must not recompute the follow state mid-animation.
    if (isCardProgrammaticScrollRef.current) return
    const el = msgScrollRef.current
    if (!el) return
    const followingLatest = el.scrollHeight - el.scrollTop - el.clientHeight <= FOLLOW_LATEST_THRESHOLD_PX
    cardFollowingLatest.current = followingLatest
    setIsCardFollowingLatest(followingLatest)
  }, [isCardProgrammaticScrollRef])

  const scrollCardToLatest = useCallback(() => {
    cardFollowingLatest.current = true
    setIsCardFollowingLatest(true)
    const el = msgScrollRef.current
    if (el) {
      markCardProgrammaticScroll()
      el.scrollTop = el.scrollHeight
    }
  }, [markCardProgrammaticScroll])
  
  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    e.stopPropagation()
    if (stillUploading || hasErrored) return
    const footer = buildAttachmentsFooter(attachments)
    const trimmed = input.trim()
    if (!trimmed && !footer) return
    onSendMessage(claw.id, trimmed + footer)
    setInput("")
    clearAttachments()
    scrollCardToLatest()
    if (cardTextareaRef.current) {
      cardTextareaRef.current.style.height = "auto"
      cardTextareaRef.current.style.overflowY = "hidden"
    }
  }

  const handleFlip = (e: React.MouseEvent) => {
    e.stopPropagation()
    setIsFlipped(!isFlipped)
  }
  
  return (
    <>
    <div
      className={cn(
        "shrink-0 relative",
        isMobile ? "w-full h-[420px]" : "w-[320px] h-full [perspective:1000px]"
      )}
    >
      <div
        className={cn(
          "relative w-full h-full",
          !isMobile && "transition-transform duration-500 [transform-style:preserve-3d]",
          !isMobile && isFlipped && "[transform:rotateY(180deg)]"
        )}
      >
        {/* Front - Chat view */}
        <div
          className={cn(
            "absolute inset-0 flex flex-col rounded-lg border border-border bg-card",
            isMobile ? isFlipped && "hidden" : "[backface-visibility:hidden]",
            hasUnread && "border-blue-500/30 bg-blue-950/10",
            isPending && "opacity-75"
          )}
          onDragEnter={isPending ? undefined : onDragEnter}
          onDragOver={isPending ? undefined : onDragOver}
          onDragLeave={onDragLeave}
          onDrop={isPending ? undefined : onDrop}
        >
          {dragHover && !isPending && (
            <div className="pointer-events-none absolute inset-0 z-30 flex items-center justify-center rounded-lg border-2 border-dashed border-ring bg-background/80">
              <div className="text-xs font-medium text-foreground">Drop files</div>
            </div>
          )}
          {isStreaming && (
            <div className="absolute left-0 top-0 bottom-0 w-1 bg-green-500 rounded-l-lg z-10" />
          )}
          {claw.status === "provisioning" && (
            <div className="absolute left-0 top-0 bottom-0 w-1 bg-blue-400 rounded-l-lg z-10 animate-pulse" />
          )}
          {claw.status === "error" && (
            <div className="absolute left-0 top-0 bottom-0 w-1 bg-red-500 rounded-l-lg z-10" />
          )}
          
          {/* Context usage bar */}
          <div className="px-3 pt-2">
            <ContextProgressBar usage={claw.contextUsage} size="sm" />
          </div>
          
          {/* Header - clickable to open full view */}
          <div className="p-3 border-b border-border">
            <div className="flex items-center gap-2 mb-1">
              {/* Drag handle — desktop board only; mobile has no reordering */}
              {dragHandleProps && (
                <span
                  {...dragHandleProps}
                  className="cursor-grab active:cursor-grabbing text-muted-foreground/40 hover:text-muted-foreground/80 transition-colors shrink-0 -ml-1"
                  title="Drag to reorder"
                  onClick={(e) => e.stopPropagation()}
                >
                  <GripVertical className="size-3.5" />
                </span>
              )}
              <StatusDot status={claw.status} isStreaming={isStreaming} />
              {claw.githubIssueUrl ? (
                <>
                  <ClawTitle
                    name={claw.name}
                    githubIssueId={claw.githubIssueId}
                    githubIssueUrl={claw.githubIssueUrl}
                    className="flex-1 font-mono text-sm font-medium text-foreground"
                  />
                  {!isPending && (
                    <button
                      onClick={() => onClick(claw.id)}
                      className="p-1 rounded hover:bg-accent transition-colors"
                      title="Open conversation"
                    >
                      <MessageSquare className="size-3.5 text-muted-foreground" />
                    </button>
                  )}
                </>
              ) : (
                <button
                  onClick={isPending ? undefined : () => onClick(claw.id)}
                  className={cn(
                    "min-w-0 font-mono text-sm font-medium text-foreground flex-1 text-left",
                    !isPending && "hover:underline"
                  )}
                >
                  <ClawTitle name={claw.name} className="block" />
                </button>
              )}
              {hasUnread && (
                <span className="px-1.5 py-0.5 text-[10px] font-medium bg-blue-500 text-white rounded-full">
                  {claw.unreadCount > 99 ? "99+" : claw.unreadCount}
                </span>
              )}
              <CopyTranscriptButton
                claw={claw}
                messages={messages}
                streamingText={streamingBuffer?.text}
                size="icon"
                stopPropagation
              />
              <button
                onClick={handleFlip}
                className="p-1 rounded hover:bg-accent transition-colors"
                title="View bot info"
              >
                <Info className="size-3.5 text-muted-foreground" />
              </button>
            </div>
            <div className="flex items-center justify-between">
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
            <BootstrapProgress claw={claw} />
            {claw.tags.length > 0 && (
              <div className="flex flex-wrap gap-1 mt-2">
                {claw.tags.slice(0, 3).map((tag) => (
                  <span
                    key={tag}
                    className="inline-flex items-center px-1.5 py-0.5 text-[10px] font-medium bg-secondary text-muted-foreground rounded"
                  >
                    {tag}
                  </span>
                ))}
                {claw.tags.length > 3 && (
                  <span className="text-[10px] text-muted-foreground">
                    +{claw.tags.length - 3}
                  </span>
                )}
              </div>
            )}
          </div>
          
          {/* Messages area */}
          <div className="flex-1 relative min-h-0 overflow-hidden">
          <div ref={msgScrollRef} onScroll={handleCardScroll} className="h-full overflow-y-auto scrollbar-thin p-3">
            {/* Content wrapper — the ResizeObserver in usePinnedAutoScroll watches it. */}
            <div ref={cardContentRef} className="space-y-2">
            {messages.length === 0 && !streamingBuffer ? (
              <p className="text-xs text-muted-foreground text-center py-4">
                No messages yet
              </p>
            ) : (
              conversationItems.map((item) => {
                if (item.type === "activity-summary") {
                  return (
                    <ActivitySummaryBlock
                      key={item.id}
                      clawId={claw.id}
                      messages={item.messages}
                      summary={item.summary}
                      density="card"
                    />
                  )
                }
                const { message } = item
                if (message.content === "__THINKING__") {
                  return (
                    <div key={message.id} className="flex gap-1 py-2 pl-2">
                      <span className="size-1.5 rounded-full bg-muted-foreground/50 animate-bounce [animation-delay:0ms]" />
                      <span className="size-1.5 rounded-full bg-muted-foreground/50 animate-bounce [animation-delay:150ms]" />
                      <span className="size-1.5 rounded-full bg-muted-foreground/50 animate-bounce [animation-delay:300ms]" />
                    </div>
                  )
                }
                if (message.role === "system") {
                  return (
                    <div key={message.id} className="flex items-center gap-2 py-1">
                      <div className="flex-1 h-px bg-border/50" />
                      <span className="text-[9px] text-muted-foreground/50 uppercase tracking-wider">
                        {message.content === "__TOOL_GAP__" ? "tool" : message.content}
                      </span>
                      <div className="flex-1 h-px bg-border/50" />
                    </div>
                  )
                }
                if (message.role === "hub") {
                  return (
                    <div key={message.id} className="flex items-start gap-1.5 py-0.5">
                      <Settings2 className="size-2.5 shrink-0 text-muted-foreground/40 mt-0.5" />
                      <span className={cn(
                        "text-[10px] italic text-muted-foreground/60 leading-tight",
                        message.format === "pre" && "whitespace-pre-wrap"
                      )}>{message.content}</span>
                    </div>
                  )
                }
                if (message.role === "activity") {
                  const step = demoteStaleRunning(pairActivitySteps([message]), false)[0]
                  return step ? <StepRow key={message.id} step={step} density="card" /> : null
                }
                const { body: cardBody, attachments: cardAttachments } = message.role === "user"
                  ? splitAttachmentsFooter(message.content)
                  : { body: message.content, attachments: [] as ParsedAttachment[] }
                return (
                  <div
                    key={message.id}
                    className={cn(
                      "text-xs p-2 rounded",
                      message.role === "user"
                        ? "bg-blue-600/20 border border-blue-500/20 ml-4"
                        : "bg-secondary mr-4"
                    )}
                  >
                    <div className="flex items-center gap-1 mb-0.5">
                      <span className="font-medium text-foreground/70">
                        {message.role === "user" ? "You" : claw.name}
                      </span>
                      <span className="text-muted-foreground" suppressHydrationWarning>
                        {formatTimestamp(message.timestamp)}
                      </span>
                    </div>
                    {cardBody.trim() && (
                      <MarkdownContent content={cardBody} className="text-xs text-foreground" />
                    )}
                    {cardAttachments.length > 0 && (
                      <div className={cn("flex flex-wrap gap-1", cardBody.trim() && "mt-1")}>
                        {cardAttachments.map((a, i) => (
                          <AttachmentChip
                            key={`${a.path}-${i}`}
                            name={a.name}
                            sizeLabel={a.sizeLabel}
                            mimetype={a.mimetype}
                            source={{ kind: "history", clawId: claw.id, path: a.path }}
                            size="sm"
                            path={a.path}
                          />
                        ))}
                      </div>
                    )}
                  </div>
                )
              })
            )}
            {latestStep && (
              <div>
                <div className="mb-0.5 text-[9px] uppercase tracking-wider text-muted-foreground/60">
                  {latestStep.status === "running" ? "Now" : "Last activity"}
                </div>
                <StepRow step={latestStep} density="card" now={activityNowMs} />
              </div>
            )}
            {streamingBuffer && (
              <StreamingMessage state={streamingBuffer} variant="card" clawName={claw.name} />
            )}
            </div>
          </div>
          {!isCardFollowingLatest && (
            <button
              type="button"
              onClick={(event) => {
                event.stopPropagation()
                scrollCardToLatest()
              }}
              className="absolute bottom-2 left-1/2 -translate-x-1/2 z-10 flex items-center gap-1 px-2.5 py-1 rounded-full bg-background/95 backdrop-blur-sm border border-border text-[10px] font-medium text-muted-foreground hover:text-foreground hover:bg-accent transition-colors shadow-md"
              aria-label="Follow latest claw activity"
            >
              <ChevronDown className="size-3" />
              <span>Latest</span>
            </button>
          )}
          </div>
          
          {/* Input area */}
          <form onSubmit={isPending ? (e) => e.preventDefault() : handleSubmit} className="p-2 border-t border-border flex flex-col gap-1.5">
            {attachments.length > 0 && (
              <div className="flex flex-wrap gap-1">
                {attachments.map((a) => (
                  <AttachmentChip
                    key={a.localId}
                    name={a.name}
                    sizeLabel={formatBytes(a.size)}
                    mimetype={a.mimetype}
                    source={a.previewUrl ? { kind: "preview", url: a.previewUrl } : undefined}
                    size="sm"
                    status={a.status}
                    error={a.error}
                    path={a.path}
                    onRemove={() => removeAttachment(a.localId)}
                  />
                ))}
              </div>
            )}
            <div className="flex gap-1.5">
              <input
                ref={cardFileInputRef}
                type="file"
                multiple
                className="hidden"
                onChange={(e) => {
                  if (e.target.files) addFiles(Array.from(e.target.files))
                  e.target.value = ""
                }}
              />
              <Button
                type="button"
                size="icon"
                variant="ghost"
                className="size-8 max-md:size-11 shrink-0"
                disabled={isPending}
                onClick={(e) => { e.stopPropagation(); cardFileInputRef.current?.click() }}
                title="Attach files"
              >
                <Paperclip className="size-3" />
                <span className="sr-only">Attach files</span>
              </Button>
              <textarea
                value={input}
                rows={1}
                onChange={(e) => {
                  setInput(e.target.value)
                  const el = e.target
                  el.style.height = "auto"
                  const maxH = 120
                  if (el.scrollHeight <= maxH) {
                    el.style.height = el.scrollHeight + "px"
                    el.style.overflowY = "hidden"
                  } else {
                    el.style.height = maxH + "px"
                    el.style.overflowY = "auto"
                  }
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault()
                    if (canSubmitCard) handleSubmit(e as unknown as React.FormEvent)
                  }
                }}
                onPaste={onPaste}
                placeholder={isPending ? (claw.status === "error" ? "Provisioning failed" : claw.status === "offline" ? "Agent offline" : "Starting up...") : "Send message..."}
                className="flex-1 resize-none overflow-hidden rounded-md border border-input bg-background px-2 py-1.5 text-xs ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 min-h-[32px]"
                disabled={isPending}
                ref={cardTextareaRef}
                onClick={(e) => e.stopPropagation()}
              />
              <Button
                type="submit"
                size="icon"
                className="size-8 max-md:size-11 shrink-0"
                disabled={!canSubmitCard}
                onClick={(e) => e.stopPropagation()}
              >
                <Send className="size-3" />
              </Button>
            </div>
          </form>
        </div>

        {/* Back - Bot info */}
        <div
          className={cn(
            "absolute inset-0 flex flex-col rounded-lg border border-border bg-card",
            isMobile
              ? !isFlipped && "hidden"
              : "[backface-visibility:hidden] [transform:rotateY(180deg)]"
          )}
        >
          {/* Header */}
          <div className="p-3 border-b border-border">
            <div className="flex items-center gap-2">
              <StatusDot status={claw.status} isStreaming={claw.isStreaming} />
              <ClawTitle
                name={claw.name}
                githubIssueId={claw.githubIssueId}
                githubIssueUrl={claw.githubIssueUrl}
                className="flex-1 font-mono text-sm font-medium text-foreground"
              />
              {claw.ssh_host && (
                <button
                  onClick={(e) => { e.stopPropagation(); setShowTerminal((v) => !v) }}
                  className={cn(
                    "p-1 rounded hover:bg-accent transition-colors",
                    showTerminal && "bg-accent text-foreground"
                  )}
                  title="Toggle terminal"
                >
                  <TerminalSquare className="size-3.5 text-muted-foreground" />
                </button>
              )}
              <button
                onClick={handleFlip}
                className="p-1 rounded hover:bg-accent transition-colors"
                title="View chat"
              >
                <MessageSquare className="size-3.5 text-muted-foreground" />
              </button>
            </div>
          </div>

          {/* Bot info content */}
          <ClawCardBack claw={claw} />

          {/* Footer */}
          <div className="p-3 border-t border-border space-y-2">
            <Button 
              variant="outline" 
              size="sm" 
              className="w-full"
              onClick={() => onClick(claw.id)}
            >
              Open Full View
            </Button>
            <div className="flex gap-2">
              <Button 
                variant="destructive" 
                size="sm" 
                className="flex-1"
                onClick={(e) => {
                  e.stopPropagation()
                  setConfirmKill(true)
                }}
              >
                <Trash2 className="size-3 mr-1.5" />
                Kill
              </Button>
            </div>
          </div>
        </div>
      </div>
    </div>
    <KillConfirmDialog clawName={claw.name} open={confirmKill} onConfirm={() => { setConfirmKill(false); onKill(claw.id) }} onCancel={() => setConfirmKill(false)} />
    {/* Terminal dialog — outside perspective container to avoid stacking context clipping */}
    {claw.ssh_host && (
      <Dialog open={showTerminal} onOpenChange={setShowTerminal}>
        <DialogContent className="!max-w-none w-[95vw] h-[90vh] flex flex-col p-0 gap-0">
          <DialogHeader className="px-4 py-3 border-b border-border shrink-0">
            <DialogTitle className="font-mono text-sm">{claw.name} — terminal</DialogTitle>
          </DialogHeader>
          <div className="flex-1 min-h-0">
            <XTerminal
              clawId={claw.id}
              wsUrl={getTerminalWsUrl(claw.id)}
              className="h-full w-full"
            />
          </div>
        </DialogContent>
      </Dialog>
    )}
    </>
  )
})

/** Sortable wrapper for ClawBoardCard */
const SortableClawBoardCard = memo(function SortableClawBoardCard({
  claw,
  messages,
  streamingBuffer,
  onClick,
  onSendMessage,
  onKill,
}: {
  claw: Claw
  messages: Message[]
  streamingBuffer?: TypewriterState
  onClick: (clawId: string) => void
  onSendMessage: (clawId: string, content: string) => void
  onKill: (clawId: string) => void
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } =
    useSortable({ id: claw.id })

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.35 : 1,
    height: "100%",
  }

  return (
    <div ref={setNodeRef} style={style} className="h-full">
      <ClawBoardCard
        claw={claw}
        messages={messages}
        streamingBuffer={streamingBuffer}
        onClick={onClick}
        onSendMessage={onSendMessage}
        onKill={onKill}
        dragHandleProps={{ ...attributes, ...listeners }}
      />
    </div>
  )
})

function formatTimestamp(date: Date): string {
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
}

const MessageBubble = memo(function MessageBubble({
  message,
  clawId,
  clawName,
  clawColor,
}: {
  message: Message
  clawId: string
  clawName: string
  clawColor?: string
}) {
  if (message.role === "system") {
    if (message.content === "__TOOL_GAP__") {
      return (
        <div className="flex items-center gap-2 py-2">
          <div className="flex-1 h-px bg-border/50" />
          <div className="flex items-center gap-1.5 text-muted-foreground/50">
            <Wrench className="size-3" />
            <span className="text-[10px] uppercase tracking-wider">tool call</span>
          </div>
          <div className="flex-1 h-px bg-border/50" />
        </div>
      )
    }
    return (
      <div className="flex items-center gap-3 py-4">
        <div className="flex-1 h-px bg-border" />
        <span className="text-xs text-muted-foreground uppercase tracking-wider font-medium">
          {message.content}
        </span>
        <div className="flex-1 h-px bg-border" />
      </div>
    )
  }

  if (message.role === "activity") {
    // Activities normally render inside turn cards; this is a defensive
    // fallback for stray rows reaching the bubble path.
    const step = demoteStaleRunning(pairActivitySteps([message]), false)[0]
    return step ? <StepRow step={step} /> : null
  }

  // Thinking indicator
  if (message.content === "__THINKING__") {
    return (
      <div className="flex justify-start">
        <div className="bg-secondary rounded-lg px-4 py-3">
          <div className="flex items-center gap-1.5">
            <span className="size-1.5 rounded-full bg-muted-foreground/60 animate-bounce [animation-delay:0ms]" />
            <span className="size-1.5 rounded-full bg-muted-foreground/60 animate-bounce [animation-delay:150ms]" />
            <span className="size-1.5 rounded-full bg-muted-foreground/60 animate-bounce [animation-delay:300ms]" />
          </div>
        </div>
      </div>
    )
  }

  const isUser = message.role === "user"
  const isHub = message.role === "hub"

  if (isHub) {
    return (
      <div className="flex items-start gap-2 py-1">
        <div className={cn(
          "flex items-start gap-1.5 text-muted-foreground/60 text-xs italic bg-muted/40 border border-border/40 rounded px-3 py-1.5 max-w-[85%]",
          message.format === "pre" && "whitespace-pre-wrap"
        )}>
          <Settings2 className="size-3 shrink-0 text-muted-foreground/50 mt-0.5" />
          <span className="text-muted-foreground/80">{message.content}</span>
        </div>
      </div>
    )
  }

  const { body, attachments: parsedAttachments } = isUser
    ? splitAttachmentsFooter(message.content)
    : { body: message.content, attachments: [] as ParsedAttachment[] }

  return (
    <div className={cn("flex w-full", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "w-fit max-w-[88%] md:w-[70%] md:max-w-none min-w-0 rounded-lg px-4 py-3",
          isUser
            ? "bg-blue-600/20 border border-blue-500/20"
            : (clawColor && COLOR_CLASSES[clawColor]?.bubble) || "bg-secondary"
        )}
      >
        <div className="flex items-center gap-2 mb-1">
          <span
            className={cn(
              "text-xs font-medium",
              isUser ? "text-muted-foreground" : "text-foreground"
            )}
          >
            {isUser ? "You" : clawName}
          </span>
          <span className="text-xs text-muted-foreground" suppressHydrationWarning>
            {formatTimestamp(message.timestamp)}
          </span>
        </div>
        {body.trim() && (
          isUser ? (
            <p className="text-sm whitespace-pre-wrap text-foreground">{body}</p>
          ) : (
            <MarkdownContent content={body} className="text-sm" />
          )
        )}
        {parsedAttachments.length > 0 && (
          <div className={cn("flex flex-wrap gap-2", body.trim() && "mt-2")}>
            {parsedAttachments.map((a, i) => (
              <AttachmentChip
                key={`${a.path}-${i}`}
                name={a.name}
                sizeLabel={a.sizeLabel}
                mimetype={a.mimetype}
                source={{ kind: "history", clawId, path: a.path }}
                size="md"
                path={a.path}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  )})

// ─── ClawChatView ─────────────────────────────────────────────────────────────
// Extracted so scroll refs are only live when this branch is mounted.

function ClawChatView({
  claw,
  messages: liveMessages,
  streamingBuffer,
  onSendMessage,
  onKill,
  onDeselectClaw,
}: {
  claw: Claw
  messages: Message[]
  streamingBuffer?: TypewriterState
  onSendMessage: (content: string) => void
  onKill: () => void
  onDeselectClaw: () => void
}) {
  const [input, setInput] = useState("")
  const [cmdToast, setCmdToast] = useState<string | null>(null)
  const [terminalOpen, setTerminalOpen] = useState(false)
  const [confirmKill, setConfirmKill] = useState(false)
  const isMobile = useIsMobile()
  const bottomRef = useRef<HTMLDivElement>(null)
  const panelTextareaRef = useRef<HTMLTextAreaElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const {
    attachments,
    dragHover,
    addFiles,
    removeAttachment,
    clearAttachments,
    onDragEnter,
    onDragOver,
    onDragLeave,
    onDrop,
    onPaste,
  } = useAttachments(claw.id)

  const {
    messages,
    hasOlder,
    loadingOlder,
    scrollRef,
    onScroll: onWindowScroll,
    isProgrammaticScrollRef,
    markProgrammaticScroll,
  } = useWindowedMessages({
    clawId: claw.id,
    liveMessages,
  })
  const [density, setDensity] = useTimelineDensity()
  const turns = useMemo(() => groupIntoTurns(messages), [messages])
  const stats = useMemo(() => timelineStats(turns), [turns])
  const runningStep = useMemo(() => latestRunningStep(turns), [turns])
  const isWorking = claw.isStreaming || Boolean(runningStep)

  // "Last output Xs ago" — the staleness signal. Live arrivals (chunks,
  // activities) are noted event-side in use-hub; the NowStrip subscribes to
  // them itself so per-chunk notifications do not re-render this panel.
  const lastMessageAt = messages.length > 0 ? messages[messages.length - 1].timestamp.getTime() : 0

  const [showScrollBtn, setShowScrollBtn] = useState(false)
  // Track whether user has scrolled away from the bottom
  const pinnedToBottom = useRef(true)
  const contentRef = useRef<HTMLDivElement>(null)

  const scrollToBottom = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    markProgrammaticScroll()
    el.scrollTop = el.scrollHeight
    pinnedToBottom.current = true
    setShowScrollBtn(false)
  }, [scrollRef, markProgrammaticScroll])

  const handleScroll = useCallback(() => {
    // Scrolls we initiate must not recompute the pin from a mid-animation position.
    if (isProgrammaticScrollRef.current) return
    const el = scrollRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
    pinnedToBottom.current = atBottom
    setShowScrollBtn(!atBottom)
    onWindowScroll()
  }, [onWindowScroll, scrollRef, isProgrammaticScrollRef])

  // Follow new rows and late content settling (markdown, images, streaming
  // growth) while pinned; never touch the scroll position otherwise.
  usePinnedAutoScroll({
    scrollRef,
    contentRef,
    pinnedRef: pinnedToBottom,
    markProgrammaticScroll,
    bottomAnchor: messages,
  })

  const isSlashCommand = (value: string, command: string) =>
    value === command || value.startsWith(`${command} `)

  const stillUploading = attachments.some((a) => a.status === "uploading")
  const hasErrored = attachments.some((a) => a.status === "error")
  const canSubmit = !stillUploading && !hasErrored && (input.trim().length > 0 || attachments.some((a) => a.status === "ready"))

  const renderMessage = useCallback(
    (message: Message) => (
      <MessageBubble
        key={message.id}
        message={message}
        clawId={claw.id}
        clawName={claw.name}
        clawColor={claw.color}
      />
    ),
    [claw.id, claw.name, claw.color]
  )

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (stillUploading || hasErrored) return
    const footer = buildAttachmentsFooter(attachments)
    const trimmed = input.trim()
    if (!trimmed && !footer) return
    setInput("")
    clearAttachments()
    pinnedToBottom.current = true
    if (panelTextareaRef.current) {
      panelTextareaRef.current.style.height = "auto"
      panelTextareaRef.current.style.overflowY = "hidden"
    }
    if (isSlashCommand(trimmed, "/cancel")) {
      setCmdToast("Hard cancel not yet implemented")
      setTimeout(() => setCmdToast(null), 3000)
      return
    }
    if (isSlashCommand(trimmed, "/stop")) {
      onSendMessage("Stop what you are doing immediately and wait for my next instruction.")
      return
    }
    const payload = trimmed + footer
    onSendMessage(payload)
  }

  return (
    <main
      className="flex-1 flex flex-col bg-background min-h-0 overflow-hidden relative"
      onDragEnter={onDragEnter}
      onDragOver={onDragOver}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
    >
      {dragHover && (
        <div className="pointer-events-none absolute inset-0 z-20 flex items-center justify-center bg-background/70 border-2 border-dashed border-ring rounded-sm">
          <div className="text-sm text-foreground font-medium">Drop files to attach</div>
        </div>
      )}
      <header className="border-b border-border">
        <div className="px-4 md:px-6 pt-2">
          <ContextProgressBar usage={claw.contextUsage} size="lg" />
        </div>
        {isMobile ? (
          /* Full-screen detail: back chevron, truncated name, actions in ⋯ */
          <div className="flex items-center gap-1 px-2 py-1.5">
            <Button variant="ghost" size="icon" onClick={onDeselectClaw} title="Back to dashboard" className="size-11 shrink-0">
              <ChevronLeft className="size-5" />
            </Button>
            <div className="min-w-0 flex-1">
              <ClawTitle
                name={claw.name}
                githubIssueId={claw.githubIssueId}
                githubIssueUrl={claw.githubIssueUrl}
                className="block font-mono text-base font-semibold text-foreground"
              />
            </div>
            <StatusBadge status={claw.status} />
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon" className="size-11 shrink-0" title="More actions">
                  <MoreVertical className="size-5" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem
                  disabled={messages.length === 0 && !streamingBuffer?.text?.trim()}
                  onClick={() => {
                    void copyTextToClipboard(
                      formatChatTranscript({ claw, messages, streamingText: streamingBuffer?.text })
                    )
                  }}
                >
                  <ClipboardCopy className="size-4" />
                  Copy transcript
                </DropdownMenuItem>
                {claw.ssh_host && (
                  <DropdownMenuItem onClick={() => setTerminalOpen(true)}>
                    <TerminalSquare className="size-4" />
                    Terminal
                  </DropdownMenuItem>
                )}
                <DropdownMenuItem variant="destructive" onClick={() => setConfirmKill(true)}>
                  <Trash2 className="size-4" />
                  Kill
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        ) : (
          <div className="flex items-center justify-between px-6 py-3">
            <div className="flex min-w-0 items-center gap-4">
              <Button variant="ghost" size="icon" onClick={onDeselectClaw} title="Back to dashboard" className="size-8">
                <LayoutGrid className="size-4" />
              </Button>
              <ClawTitle
                name={claw.name}
                githubIssueId={claw.githubIssueId}
                githubIssueUrl={claw.githubIssueUrl}
                className="flex-1 font-mono text-xl font-semibold text-foreground"
              />
              <StatusBadge status={claw.status} />
              <span className="text-sm text-muted-foreground font-mono">{formatUptime(claw.uptime)}</span>
            </div>
            <div className="flex items-center gap-2">
              <CopyTranscriptButton
                claw={claw}
                messages={messages}
                streamingText={streamingBuffer?.text}
                size="sm"
              />
              {claw.ssh_host && (
                <Button variant="outline" size="sm" onClick={() => setTerminalOpen(true)}>
                  <TerminalSquare className="size-3.5 mr-1.5" />
                  Terminal
                </Button>
              )}
              <Button variant="destructive" size="sm" onClick={() => setConfirmKill(true)}>Kill</Button>
            </div>
          </div>
        )}
        <BootstrapProgress claw={claw} variant="full" />
      </header>

      {isWorking && (
        <NowStrip clawId={claw.id} step={runningStep} isStreaming={claw.isStreaming} lastMessageAt={lastMessageAt} />
      )}
      <TimelineToolbar density={density} onDensityChange={setDensity} stats={stats} />

      <div ref={scrollRef} onScroll={handleScroll} className="flex-1 overflow-y-auto scrollbar-thin p-4 md:p-6 relative">
        <div ref={contentRef} className="space-y-4 max-w-3xl mx-auto">
          {loadingOlder && (
            <div className="flex justify-center py-2">
              <span className="text-xs text-muted-foreground animate-pulse">Loading older messages...</span>
            </div>
          )}
          {hasOlder && !loadingOlder && (
            <div className="flex justify-center py-1">
              <div className="h-px w-full bg-border" />
            </div>
          )}
          {messages.length === 0 && !streamingBuffer ? (
            <p className="text-center text-muted-foreground py-12">No messages yet. Start the conversation below.</p>
          ) : (
            <AgentTimeline
              clawId={claw.id}
              turns={turns}
              density={density}
              renderMessage={renderMessage}
              isWorking={isWorking}
              streamingSlot={
                streamingBuffer ? (
                  <StreamingMessage
                    state={streamingBuffer}
                    variant="chat"
                    clawName={claw.name}
                    clawColor={claw.color}
                  />
                ) : undefined
              }
              scrollRef={scrollRef}
              pinnedRef={pinnedToBottom}
              markProgrammaticScroll={markProgrammaticScroll}
            />
          )}
          <div ref={bottomRef} className="h-4" />
        </div>
        {showScrollBtn && (
          <button
            onClick={scrollToBottom}
            className="absolute bottom-4 left-1/2 -translate-x-1/2 z-10 flex items-center gap-1.5 px-3 py-1.5 rounded-full bg-secondary border border-border text-xs text-muted-foreground hover:text-foreground hover:bg-accent transition-colors shadow-md"
          >
            <ChevronDown className="size-3.5" />
            <span>Scroll to bottom</span>
          </button>
        )}
      </div>

      {/* Composer — padded above the home indicator on notched phones */}
      <div className="p-4 border-t border-border pb-[calc(1rem+env(safe-area-inset-bottom))] md:pb-4">
        {cmdToast && (
          <div className="mb-2 max-w-3xl mx-auto text-xs text-amber-400 bg-amber-500/10 border border-amber-500/20 rounded-md px-3 py-2">
            {cmdToast}
          </div>
        )}
        <form
          onSubmit={handleSubmit}
          className="flex flex-col gap-2 max-w-3xl mx-auto rounded-md"
        >
          {attachments.length > 0 && (
            <div className="flex flex-wrap gap-2">
              {attachments.map((a) => (
                <AttachmentChip
                  key={a.localId}
                  name={a.name}
                  sizeLabel={formatBytes(a.size)}
                  mimetype={a.mimetype}
                  source={a.previewUrl ? { kind: "preview", url: a.previewUrl } : undefined}
                  size="md"
                  status={a.status}
                  error={a.error}
                  path={a.path}
                  onRemove={() => removeAttachment(a.localId)}
                />
              ))}
            </div>
          )}
          <div className="flex gap-2 items-end">
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              onChange={(e) => {
                if (e.target.files) addFiles(Array.from(e.target.files))
                e.target.value = ""
              }}
            />
            <Button
              type="button"
              size="icon"
              variant="ghost"
              onClick={() => fileInputRef.current?.click()}
              className="shrink-0 max-md:size-11"
              title="Attach files"
            >
              <Paperclip className="size-4" />
              <span className="sr-only">Attach files</span>
            </Button>
            <textarea
              value={input}
              onChange={(e) => {
                setInput(e.target.value)
                const el = e.target
                el.style.height = "auto"
                const maxH = 200
                if (el.scrollHeight <= maxH) {
                  el.style.height = el.scrollHeight + "px"
                  el.style.overflowY = "hidden"
                } else {
                  el.style.height = maxH + "px"
                  el.style.overflowY = "auto"
                }
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault()
                  if (canSubmit) handleSubmit(e as unknown as React.FormEvent)
                }
              }}
              onPaste={onPaste}
              ref={panelTextareaRef}
              placeholder="Message agent, /stop, or attach files"
              rows={1}
              className="flex-1 resize-none overflow-hidden rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 min-h-[40px]"
            />
            <Button type="submit" size="icon" disabled={!canSubmit} className="shrink-0 max-md:size-11">
              <Send className="size-4" />
              <span className="sr-only">Send message</span>
            </Button>
          </div>
        </form>
      </div>

      <KillConfirmDialog clawName={claw.name} open={confirmKill} onConfirm={() => { setConfirmKill(false); onKill() }} onCancel={() => setConfirmKill(false)} />

      {/* Terminal dialog */}
      {claw.ssh_host && (
        <Dialog open={terminalOpen} onOpenChange={setTerminalOpen}>
          <DialogContent className="!max-w-none w-[95vw] h-[90vh] flex flex-col p-0 gap-0">
            <DialogHeader className="px-4 py-3 border-b border-border shrink-0">
              <DialogTitle className="font-mono text-sm">{claw.name} — terminal</DialogTitle>
            </DialogHeader>
            <div className="flex-1 min-h-0">
              {terminalOpen && (
                <XTerminal
                  clawId={claw.id}
                  wsUrl={getTerminalWsUrl(claw.id)}
                  className="h-full w-full"
                />
              )}
            </div>
          </DialogContent>
        </Dialog>
      )}
    </main>
  )
}

// ─── ConversationView ─────────────────────────────────────────────────────────

export function ConversationView({
  claw,
  allClaws,
  downtimeDependencies,
  loading = false,
  hubError = null,
  messages,
  allMessages,
  streamingBuffers,
  onSendMessage,
  onSendMessageToClaw,
  onKill,
  onKillClaw,
  onSelectClaw,
  onDeselectClaw,
  onReorderClaws,
  onOpenMenu,
}: ConversationViewProps) {
  const boardRef = useRef<HTMLDivElement>(null)
  const [activeDragClaw, setActiveDragClaw] = useState<Claw | null>(null)
  const { logoUrl } = useBranding()
  const isMobile = useIsMobile()
  const handleCardClick = useCallback((clawId: string) => onSelectClaw(clawId), [onSelectClaw])
  const handleCardSendMessage = useCallback((clawId: string, content: string) => onSendMessageToClaw(clawId, content), [onSendMessageToClaw])
  const handleCardKill = useCallback((clawId: string) => onKillClaw(clawId), [onKillClaw])

  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: { distance: 6 },
    })
  )

  function handleBoardDragStart(event: DragStartEvent) {
    const found = allClaws.find((c) => c.id === event.active.id)
    setActiveDragClaw(found ?? null)
  }

  function handleBoardDragEnd(event: DragEndEvent) {
    setActiveDragClaw(null)
    const { active, over } = event
    if (!over || active.id === over.id) return
    const ids = allClaws.map((c) => c.id)
    const oldIdx = ids.indexOf(active.id as string)
    const newIdx = ids.indexOf(over.id as string)
    onReorderClaws(arrayMove(ids, oldIdx, newIdx))
  }

  // On initial load, scroll board to leftmost active card
  useEffect(() => {
    if (!boardRef.current) return
    boardRef.current.scrollLeft = 0
  }, [])

  const scrollBoard = (direction: "left" | "right") => {
    if (boardRef.current) {
      const scrollAmount = 340
      boardRef.current.scrollBy({
        left: direction === "left" ? -scrollAmount : scrollAmount,
        behavior: "smooth",
      })
    }
  }

  if (hubError) {
    return (
      <main className="flex-1 flex flex-col bg-background min-w-0 overflow-hidden">
        <div className="flex flex-col items-center justify-center h-full gap-4 text-center px-8">
          <div className="rounded-full bg-red-500/10 p-4">
            <AlertCircle className="size-8 text-red-500" />
          </div>
          <div className="space-y-2">
            <p className="text-base font-medium text-foreground">Cannot reach the hub</p>
            <p className="text-sm text-muted-foreground max-w-sm">Make sure <code className="bg-muted px-1 rounded text-xs">ELASTICCLAW_HUB_URL</code> and <code className="bg-muted px-1 rounded text-xs">ELASTICCLAW_HUB_TOKEN</code> are set correctly.</p>
            <a href="/api/debug" target="_blank" rel="noopener" className="text-xs text-blue-400 hover:underline">
              View debug info →
            </a>
          </div>
        </div>
      </main>
    )
  }

  if (!claw) {
    // Use the server-maintained order (respects user drag preference + falls back to API order)
    const sortedClaws = allClaws

    return (
      <main className="flex-1 flex flex-col bg-background min-w-0 overflow-hidden">
        {/* Header */}
        <header className="flex items-center justify-between gap-2 px-3 md:px-6 py-2 md:py-4 border-b border-border shrink-0">
          <div className="flex min-w-0 items-center gap-1 md:gap-3">
            {isMobile && onOpenMenu ? (
              <Button variant="ghost" size="icon" className="size-11 shrink-0" onClick={onOpenMenu} title="Open agent list">
                <Menu className="size-5" />
              </Button>
            ) : (
              <Terminal className="size-5 text-muted-foreground" />
            )}
            <h2 className="truncate text-lg font-medium text-foreground">
              {loading ? "Agents" : `${allClaws.length} Active Agents`}
            </h2>
          </div>
          <div className="flex min-w-0 flex-wrap items-center justify-end gap-x-4 gap-y-2 text-xs text-muted-foreground">
            <DependencyDowntimeBanner dependencies={downtimeDependencies} />
            <div className="hidden md:flex items-center gap-4">
              <div className="flex items-center gap-1.5">
                <span className="size-2 rounded-full bg-green-500" />
                <span>Connected</span>
              </div>
              <div className="flex items-center gap-1.5">
                <span className="size-2 rounded-full bg-amber-500" />
                <span>Idle</span>
              </div>
              <div className="flex items-center gap-1.5">
                <span className="size-2 rounded-full bg-red-500" />
                <span>Offline</span>
              </div>
            </div>
            {isMobile && (
              /* Sign out moves here on mobile — the tab bar has no room for it */
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon" className="size-11 shrink-0" title="More">
                    <MoreVertical className="size-5" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => { void signOut() }}>
                    <LogOut className="size-4" />
                    Sign out
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            )}
          </div>
        </header>

        {/* Board view */}
        <div className="flex-1 relative min-h-0">
          {sortedClaws.length === 0 && !loading ? (
            <div className="flex flex-col items-center justify-center h-full gap-6 px-8 text-center">
              <img
                src={logoUrl || "/mascot.png?v=2"}
                alt="mascot"
                className="w-72 h-72 object-contain select-none pointer-events-none opacity-90"
                draggable={false}
              />
              <div className="space-y-2">
                <p className="text-lg font-medium text-muted-foreground">No agents running</p>
                <p className="text-sm text-muted-foreground/70 max-w-sm">
                  Start your first agent from the CLI to get started.
                </p>
              </div>
              <div className="bg-muted rounded-lg px-4 py-3 font-mono text-sm text-foreground/80 max-w-md w-full text-left">
                <span className="text-muted-foreground select-none">$ </span>
                elasticclaw create --name my-agent
              </div>
            </div>
          ) : isMobile ? (
            /* Single-column vertical list: full-width cards, no reordering,
               no scroll arrows — one-finger vertical scrolling only. */
            <div className="h-full overflow-y-auto overflow-x-hidden p-3 flex flex-col gap-3">
              {sortedClaws.map((c) => (
                <ClawBoardCard
                  key={c.id}
                  claw={c}
                  messages={allMessages[c.id] ?? EMPTY_MESSAGES}
                  streamingBuffer={streamingBuffers[c.id]}
                  onClick={handleCardClick}
                  onSendMessage={handleCardSendMessage}
                  onKill={handleCardKill}
                />
              ))}
            </div>
          ) : (
          <>
          <Button
            variant="ghost"
            size="icon"
            className="absolute left-2 top-1/2 -translate-y-1/2 z-10 bg-background/80 backdrop-blur-sm border border-border shadow-sm"
            onClick={() => scrollBoard("left")}
          >
            <ChevronLeft className="size-4" />
          </Button>

          <DndContext
            sensors={sensors}
            collisionDetection={closestCenter}
            onDragStart={handleBoardDragStart}
            onDragEnd={handleBoardDragEnd}
          >
            <SortableContext
              items={sortedClaws.map((c) => c.id)}
              strategy={horizontalListSortingStrategy}
            >
              <div
                ref={boardRef}
                className="flex gap-4 h-full overflow-x-auto overflow-y-hidden py-6 px-12 items-stretch"
                style={{ scrollbarWidth: "none", msOverflowStyle: "none" }}
              >
                {sortedClaws.map((c) => (
                  <SortableClawBoardCard
                    key={c.id}
                    claw={c}
                    messages={allMessages[c.id] ?? EMPTY_MESSAGES}
                    streamingBuffer={streamingBuffers[c.id]}
                    onClick={handleCardClick}
                    onSendMessage={handleCardSendMessage}
                    onKill={handleCardKill}
                  />
                ))}
              </div>
            </SortableContext>

            {/* Ghost card following cursor during drag */}
            <DragOverlay>
              {activeDragClaw ? (
                <div className="opacity-90 shadow-2xl h-full" style={{ width: 320 }}>
                  <ClawBoardCard
                    claw={activeDragClaw}
                    messages={allMessages[activeDragClaw.id] ?? EMPTY_MESSAGES}
                    streamingBuffer={streamingBuffers[activeDragClaw.id]}
                    onClick={noopClawAction}
                    onSendMessage={noopClawMessageAction}
                    onKill={noopClawAction}
                  />
                </div>
              ) : null}
            </DragOverlay>
          </DndContext>

          <Button
            variant="ghost"
            size="icon"
            className="absolute right-2 top-1/2 -translate-y-1/2 z-10 bg-background/80 backdrop-blur-sm border border-border shadow-sm"
            onClick={() => scrollBoard("right")}
          >
            <ChevronRight className="size-4" />
          </Button>
          </>
          )}
        </div>
      </main>
    )
  }

  return (
    <ClawChatView
      key={claw.id}
      claw={claw}
      messages={messages}
      streamingBuffer={streamingBuffers[claw.id]}
      onSendMessage={onSendMessage}
      onKill={onKill}
      onDeselectClaw={onDeselectClaw}
    />
  )
}
