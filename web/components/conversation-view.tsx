"use client"

import { useState, useRef, useEffect, useCallback, useMemo, memo } from "react"
import { Send, Terminal, TerminalSquare, ChevronLeft, ChevronRight, ChevronDown, Loader2, LayoutGrid, Info, Trash2, AlertCircle, Wrench, GripVertical, Settings2, Paperclip, File as FileIcon, X, Menu, MoreVertical, LogOut, ClipboardCopy, CheckCircle2, GitPullRequest, Bot } from "lucide-react"
import {
  compactActivityRuns,
  demoteStaleRunning,
  groupIntoTurns,
  latestRunningStep,
  pairActivitySteps,
  timelineStats,
  trailingActivityRun,
  type Step,
} from "@/lib/turns"
import type { NowStripState } from "@/components/agent-timeline/now-strip"
import { AgentTimeline } from "@/components/agent-timeline/timeline"
import { TimelineToolbar, useTimelineDensity } from "@/components/agent-timeline/timeline-toolbar"
import { NowStrip } from "@/components/agent-timeline/now-strip"
import { StepRow } from "@/components/agent-timeline/step-row"
import { ActivitySummaryBlock } from "@/components/agent-timeline/activity-summary-block"
import { SubagentRail } from "@/components/agent-timeline/subagent-rail"
import { SubagentLanes } from "@/components/agent-timeline/subagent-lanes"
import { SubagentDetail } from "@/components/agent-timeline/subagent-detail"
import { useSubagentView, type SubagentView } from "@/components/agent-timeline/use-subagent-view"
import { collectSubagents } from "@/lib/subagents"
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
  CollisionDetection,
} from "@dnd-kit/core"
import {
  SortableContext,
  useSortable,
  horizontalListSortingStrategy,
  arrayMove,
} from "@dnd-kit/sortable"
import { CSS } from "@dnd-kit/utilities"
import { MarkdownContent } from "@/components/markdown-content"
import { COLOR_CLASSES, CLAW_COLORS } from "@/lib/mappers"
import { TagEditor } from "@/components/tag-editor"
import { useWindowedMessages } from "@/hooks/use-windowed-messages"
import { useProgrammaticScrollFlag, usePinnedAutoScroll } from "@/hooks/use-pinned-scroll"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
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
import { getTerminalWsUrl, fetchClawPRs, patchClaw, type ClawPR } from "@/lib/api"
import { buildAttachmentsFooter, splitAttachmentsFooter, formatBytes, type ParsedAttachment } from "@/lib/attachments"
import { useAttachments } from "@/hooks/use-attachments"
import { AttachmentChip } from "@/components/attachment-chip"
import dynamic from "next/dynamic"
import { useBranding } from "@/hooks/use-branding"
import { BootstrapProgress } from "@/components/bootstrap-progress"
import { AGENT_SECTION, agentSection, type AgentSectionName } from "@/components/ds/agent-section"
import { CardAction } from "@/components/ds/card-action"
import { ClawTitle } from "@/components/claw-title"
import { windowMessagesByDurableCount } from "@/lib/messages"
import { DependencyDowntimeBanner } from "@/components/dependency-downtime-banner"
import type { TypewriterState } from "@/hooks/use-typewriter"
import { extractQuestion, isWaitingOnYou } from "@/lib/waiting-on-you"
import { messageAuthor } from "@/lib/message-author"
import { toast } from "@/hooks/use-toast"

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
  currentUserLogin?: string | null
  currentUserResolved: boolean
  /** Mobile only: opens the sidebar drawer from the board header hamburger. */
  onOpenMenu?: () => void
}

const FOLLOW_LATEST_THRESHOLD_PX = 24
/** Last N durable conversation turns on board cards (activities do not count). */
const BOARD_CARD_DURABLE_MESSAGE_WINDOW = 50
const EMPTY_MESSAGES: Message[] = []
const noopClawAction = (_clawId: string) => {}
const noopClawMessageAction = (_clawId: string, _content: string) => {}
const clawPRCache = new Map<string, ClawPR[]>()
const clawPRRequests = new Map<string, Promise<ClawPR[]>>()

function fetchCachedClawPRs(clawId: string) {
  const pending = clawPRRequests.get(clawId)
  if (pending) return pending
  const request = fetchClawPRs(clawId)
    .then((prs) => {
      const openPrs = prs.filter((pr) => pr.state === "open")
      clawPRCache.set(clawId, openPrs)
      return openPrs
    })
    .finally(() => clawPRRequests.delete(clawId))
  clawPRRequests.set(clawId, request)
  return request
}

function useClawPRs(clawId: string, enabled: boolean) {
  const [prs, setPrs] = useState<ClawPR[]>(() => clawPRCache.get(clawId) ?? [])
  const [hasLoaded, setHasLoaded] = useState(() => clawPRCache.has(clawId))
  const [loadedClawId, setLoadedClawId] = useState(clawId)
  if (loadedClawId !== clawId) {
    setLoadedClawId(clawId)
    setPrs(clawPRCache.get(clawId) ?? [])
    setHasLoaded(clawPRCache.has(clawId))
  }

  useEffect(() => {
    if (!enabled) return
    let cancelled = false
    void fetchCachedClawPRs(clawId).then((nextPRs) => {
      if (cancelled) return
      setPrs(nextPRs)
      setHasLoaded(true)
    }).catch(() => {})
    return () => { cancelled = true }
  }, [clawId, enabled])

  return { prs, hasLoaded }
}

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

function firstMeaningfulLine(text: string): string {
  for (const line of text.split("\n")) {
    const trimmed = line.replace(/^#+\s*/, "").trim()
    if (trimmed) return trimmed
  }
  return text.trim()
}

function lastActivityError(messages: Message[]): string | null {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const m = messages[i]
    if (m.role === "activity" && m.activity?.error) return m.activity.error
  }
  return null
}

interface BoardCardNow {
  state: NowStripState
  text?: string
  at?: number | null
}

/**
 * The card's status line: is the agent working, waiting on the user, finished,
 * broken, or gone — and the one-liner that proves it. Working defers to the
 * NowStrip's live content (tool + elapsed + last-output age).
 */
function boardCardNow(
  claw: Claw,
  messages: Message[],
  latestStep: Step | null,
  isStreaming: boolean
): BoardCardNow | null {
  // BootstrapProgress owns the provisioning story.
  if (claw.status === "provisioning") return null

  let lastAt: number | null = null
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i].role === "system") continue
    lastAt = messages[i].timestamp.getTime()
    break
  }

  if (claw.status === "error") {
    return {
      state: "error",
      text: claw.reason || lastActivityError(messages) || "Agent errored",
      at: lastAt,
    }
  }
  if (claw.status === "offline") {
    const seen = claw.last_seen ? new Date(claw.last_seen).getTime() : NaN
    return { state: "offline", at: Number.isFinite(seen) ? seen : lastAt }
  }
  if (isStreaming || latestStep?.status === "running") return { state: "working" }

  // Nothing live: surface what the agent ended with. Transcript tail decides —
  // trailing tool activity beats older prose, a question flags "needs you".
  if (latestStep) {
    return {
      state: "done",
      text: latestStep.detail ? `${latestStep.title} · ${latestStep.detail}` : latestStep.title,
      at: (latestStep.endedAt ?? latestStep.startedAt).getTime(),
    }
  }
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const m = messages[i]
    if (m.role === "system" || m.role === "activity" || m.role === "activity_summary") continue
    const at = m.timestamp.getTime()
    if (m.role === "claw") {
      const text = m.content.trim()
      const question = extractQuestion(text)
      if (question) return { state: "waiting", text: question, at }
      return { state: "done", text: firstMeaningfulLine(text), at }
    }
    if (m.role === "hub") return { state: "done", text: m.content, at }
    if (m.role === "user") return { state: "done", text: "Waiting to start", at }
  }
  return { state: "done", text: "Idle", at: lastAt }
}

function formatUptime(seconds: number): string {
  if (seconds === 0) return "—"
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  const hours = Math.floor(seconds / 3600)
  const mins = Math.floor((seconds % 3600) / 60)
  return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`
}

function StatusBadge({ status, className }: { status: ClawStatus; className?: string }) {
  const color = status === "connected" ? "var(--status-connected)" : status === "idle" ? "var(--status-idle)" : status === "provisioning" ? "var(--status-provisioning)" : status === "error" ? "var(--status-error)" : "var(--status-offline)"
  return (
    <Badge
      variant="outline"
      className={cn("text-xs font-medium", className)}
      style={{ color, borderColor: `color-mix(in srgb, ${color} 50%, transparent)` }}
    >
      {status}
    </Badge>
  )
}

function StatusDot({ status, isStreaming }: { status: ClawStatus; isStreaming: boolean }) {
  if (isStreaming) return <Loader2 className="size-3.5 animate-spin" style={{ color: "var(--status-streaming)" }} />
  if (status === "provisioning") return <Loader2 className="size-3.5 animate-spin" style={{ color: "var(--status-provisioning)" }} />
  if (status === "error") return <AlertCircle className="size-3.5" style={{ color: "var(--status-error)" }} />
  const color = status === "connected" ? "var(--status-connected)" : status === "idle" ? "var(--status-idle)" : "var(--status-offline)"
  return (
    <span className="size-2 rounded-full shrink-0" style={{ backgroundColor: color }} />
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

function ClawCardBack({ claw, open }: { claw: Claw; open: boolean }) {
  const { prs } = useClawPRs(claw.id, open)
  const [localTags, setLocalTags] = useState(claw.tags)
  const [localName, setLocalName] = useState(claw.name)
  const [syncedClaw, setSyncedClaw] = useState(claw)

  if (syncedClaw.id !== claw.id || syncedClaw.tags !== claw.tags || syncedClaw.name !== claw.name) {
    setSyncedClaw(claw)
    setLocalTags(claw.tags)
    setLocalName(claw.name)
  }

  return (
    /* max-md cap mirrors the front face's message list: mobile cards are
       content-sized, so the info panel scrolls inside its own bound. */
    <div className="flex-1 overflow-y-auto scrollbar-thin p-4 space-y-4 max-md:max-h-[40vh]">
      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">
          Purpose
        </h3>
        <p className="text-sm text-foreground leading-relaxed">
          {claw.description || "No description provided for this agent."}
        </p>
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">
          Source
        </h3>
        <p className="text-sm font-mono text-foreground">
          {claw.template}
        </p>
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">
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
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">
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
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">
          Uptime
        </h3>
        <p className="text-sm font-mono text-foreground">
          {formatUptime(claw.uptime)}
        </p>
      </div>

      {/* Editing moved here from the sidebar row, which stays read-only per
          the kit AgentRow. */}
      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">Name</h3>
        <input
          value={localName}
          onChange={(e) => setLocalName(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") (e.target as HTMLInputElement).blur() }}
          onBlur={(e) => {
            const name = localName.trim()
            if (!name || name === claw.name) {
              setLocalName(claw.name)
              return
            }
            patchClaw(claw.id, { name }).catch(() => {
              setLocalName(claw.name)
              toast({ variant: "destructive", title: "Unable to rename agent" })
            })
          }}
          className="w-full rounded-md border border-input bg-input/30 px-2.5 py-1.5 font-mono text-sm outline-none focus-visible:border-ring"
        />
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">Tags</h3>
        <TagEditor clawId={claw.id} tags={localTags} onTagsChange={setLocalTags} />
      </div>

      <div>
        <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">Color</h3>
        <div className="flex flex-wrap gap-1.5">
          {CLAW_COLORS.map((color) => (
            <button
              key={color}
              onClick={() => { patchClaw(claw.id, { color }).catch((err) => console.error("Failed to update color", err)) }}
              className={cn(
                "size-4 rounded-full transition-transform hover:scale-125",
                COLOR_CLASSES[color]?.dot,
                claw.color === color && "ring-2 ring-offset-1 ring-offset-background ring-foreground"
              )}
              title={color}
            />
          ))}
        </div>
      </div>

      {prs.length > 0 && (
        <div>
          <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-[0.08em] mb-2">Pull Requests</h3>
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
  currentUserLogin,
  currentUserResolved,
}: { 
  claw: Claw
  messages: Message[]
  streamingBuffer?: TypewriterState
  onClick: (clawId: string) => void
  onSendMessage: (clawId: string, content: string) => void
  onKill: (clawId: string) => void
  dragHandleProps?: React.HTMLAttributes<HTMLElement>
  currentUserLogin?: string | null
  currentUserResolved: boolean
}) {
  const [input, setInput] = useState("")
  const cardTextareaRef = useRef<HTMLTextAreaElement>(null)
  const cardFileInputRef = useRef<HTMLInputElement>(null)
  const isMobile = useIsMobile()
  const [showTerminal, setShowTerminal] = useState(false)
  const [confirmKill, setConfirmKill] = useState(false)
  const [detailsOpen, setDetailsOpen] = useState(false)
  const [copied, setCopied] = useState(false)
  const [prsOpen, setPrsOpen] = useState(false)
  const { prs, hasLoaded: hasLoadedPrs } = useClawPRs(claw.id, prsOpen)
  const openPRCount = prsOpen && hasLoadedPrs ? prs.length : claw.openPrCount
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
  // An offline/errored claw cannot still be running its dangling last step.
  const allowTrailingRunning = claw.status !== "offline" && claw.status !== "error"
  // Latest step of the trailing activity run — drives the card's status line
  // (paired start/terminal, live elapsed while running).
  const latestStep = useMemo(() => {
    const steps = demoteStaleRunning(
      pairActivitySteps(trailingActivityRun(visibleMessages)),
      allowTrailingRunning
    )
    return steps.length > 0 ? steps[steps.length - 1] : null
  }, [visibleMessages, allowTrailingRunning])
  const activityNowMs = useNowTick(Boolean(latestStep))
  const isStreaming = claw.isStreaming || Boolean(streamingBuffer?.hadChunks && streamingBuffer.text)
  const waitingOnYou = useMemo(() => isWaitingOnYou(messages), [messages])
  const cardNow = useMemo(
    () => boardCardNow(claw, visibleMessages, latestStep, isStreaming),
    [claw, visibleMessages, latestStep, isStreaming]
  )
  const runningStep = latestStep?.status === "running" ? latestStep : null
  const lastMessageAt = messages.length > 0 ? messages[messages.length - 1].timestamp.getTime() : 0
  // Footer stat line: steps and failures over the loaded window, plus whatever
  // is still summarized behind unexpanded placeholders.
  const cardStats = useMemo(() => {
    const steps = pairActivitySteps(visibleMessages)
    let toolCalls = 0
    let failures = 0
    for (const step of steps) {
      if (step.kind === "tool") toolCalls += 1
      if (step.status === "failed") failures += 1
    }
    for (const m of visibleMessages) {
      if (m.role === "activity_summary") toolCalls += m.activitySummary?.count ?? 0
    }
    return { toolCalls, failures }
  }, [visibleMessages])

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

  const copyTranscript = useCallback(async (event: React.MouseEvent) => {
    event.stopPropagation()
    await copyTextToClipboard(formatChatTranscript({ claw, messages, streamingText: streamingBuffer?.text }))
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1400)
  }, [claw, messages, streamingBuffer?.text])
  
  return (
    <>
    <div
      className={cn(
        "shrink-0 relative",
        // Mobile cards size to their content (capped below) instead of a
        // fixed desktop-carryover height — several agents fit per screen.
        isMobile ? "w-full" : "w-[500px] h-full"
      )}
    >
      <div className="relative h-full w-full">
        {/* Chat view. On mobile it is in normal flow and sizes to its
            content; the message list below carries its own viewport cap and
            scrolls inside, so the whole card stays around 60vh at most. (A
            max-height on the card itself would not work: `h-full` inside a
            max-height-clamped auto container resolves against an indefinite
            height, so the inner scroller would overflow and get clipped
            instead of scrolling.) */}
        <div
          className={cn(
            "flex flex-col rounded-lg border border-border bg-card",
            isMobile ? "relative" : "absolute inset-0",
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
          <div className="absolute left-0 top-0 bottom-0 w-1 rounded-l-lg z-10" style={{ backgroundColor: isStreaming ? "var(--status-streaming)" : claw.status === "provisioning" ? "var(--status-provisioning)" : claw.status === "error" ? "var(--status-error)" : AGENT_SECTION[agentSection(claw, { isWaitingOnYou: waitingOnYou })].color }} />
          
          {/* Context usage bar */}
          <div className="px-3 pt-2">
            <ContextProgressBar usage={claw.contextUsage} size="sm" />
          </div>
          
          {/* Header - clickable to open full view */}
          <div className="p-3 border-b border-border">
            <div className="flex items-start gap-2 mb-1">
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
              <div className="min-w-0 flex-1">
                <div className="flex min-w-0 items-center gap-2">
                  <StatusDot status={claw.status} isStreaming={isStreaming} />
                  <button onClick={isPending ? undefined : () => onClick(claw.id)} className="min-w-0 flex-1 text-left font-mono text-sm font-medium text-foreground hover:underline">
                    <ClawTitle name={claw.name} githubIssueId={claw.githubIssueId} githubIssueUrl={claw.githubIssueUrl} className="block" />
                  </button>
                  {hasUnread && <span className="rounded-full bg-blue-500 px-1.5 py-0.5 text-[10px] font-medium text-white">{claw.unreadCount > 99 ? "99+" : claw.unreadCount}</span>}
                </div>
                <div className="mt-1 flex items-center gap-1 text-xs text-muted-foreground"><span className="truncate">{claw.template}</span><span>·</span><span className={cn("font-mono shrink-0", claw.status === "provisioning" && "text-[var(--status-provisioning)]", claw.status === "error" && "text-[var(--status-error)]")}>{claw.status === "provisioning" ? "starting..." : claw.status === "error" ? "error" : formatUptime(claw.uptime)}</span></div>
              </div>
              <div className="flex shrink-0 items-center gap-1 border-l border-border pl-2">
                {(openPRCount > 0 || prsOpen) && <Popover open={prsOpen} onOpenChange={setPrsOpen}><PopoverTrigger asChild><CardAction icon={GitPullRequest} label={`Open ${openPRCount} pull request${openPRCount === 1 ? "" : "s"}`} count={openPRCount} tone="var(--chart-1)" onClick={(event) => event.stopPropagation()} /></PopoverTrigger><PopoverContent className="w-72 p-2" align="end"><span className="block px-2 py-1 text-[10px] font-medium uppercase tracking-[0.08em] text-muted-foreground">Open pull requests</span>{prs.length > 0 ? prs.map((pr) => <a key={pr.id} href={pr.url} target="_blank" rel="noopener noreferrer" onClick={(event) => event.stopPropagation()} className="block rounded px-2 py-1 hover:bg-accent" aria-label={`Open ${pr.repo} pull request #${pr.prNumber}: ${pr.title}`} title={`${pr.repo}#${pr.prNumber}: ${pr.title}`}><div className="font-mono text-xs text-[var(--chart-1)]">{pr.repo}#{pr.prNumber}</div><div className="truncate text-xs text-muted-foreground">{pr.title}</div></a>) : <p className="px-2 py-1 text-xs text-muted-foreground">No open pull requests.</p>}</PopoverContent></Popover>}
                <CardAction icon={copied ? CheckCircle2 : ClipboardCopy} label={copied ? "Transcript copied" : "Copy transcript"} confirmed={copied} onClick={copyTranscript} />
                <CardAction icon={Info} label="Agent details" onClick={(event) => { event.stopPropagation(); setDetailsOpen(true) }} />
              </div>
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

          {/* Status line — what this agent is doing, with no clicks */}
          {cardNow && (
            <NowStrip
              clawId={claw.id}
              step={runningStep}
              isStreaming={isStreaming}
              lastMessageAt={lastMessageAt}
              variant="card"
              state={cardNow.state}
              statusText={cardNow.text}
              statusAt={cardNow.at}
            />
          )}

          {/* Messages area */}
          <div className="flex-1 relative min-h-0 overflow-hidden">
          <div
            ref={msgScrollRef}
            onScroll={handleCardScroll}
            className={cn(
              "overflow-y-auto scrollbar-thin p-3",
              // vh cap so mobile cards are content-sized with an internal
              // scroll; desktop fills the fixed-height card as before.
              isMobile ? "max-h-[40vh]" : "h-full"
            )}
          >
            {/* Content wrapper — the ResizeObserver in usePinnedAutoScroll watches it. */}
            <div ref={cardContentRef} className="space-y-2">
            {messages.length === 0 && !streamingBuffer ? (
              <p className="text-xs text-muted-foreground text-center py-4">
                No messages yet
              </p>
            ) : (
              conversationItems.map((item, index) => {
                if (item.type === "activity-summary") {
                  return (
                    <ActivitySummaryBlock
                      key={item.id}
                      clawId={claw.id}
                      messages={item.messages}
                      summary={item.summary}
                      density="card"
                      now={activityNowMs}
                      keepTrailingRunning={
                        allowTrailingRunning && index === conversationItems.length - 1
                      }
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
                      <span className="text-[9px] text-muted-foreground/50 uppercase tracking-[0.08em]">
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
                const author = messageAuthor(message, currentUserLogin, currentUserResolved)
                const { body: cardBody, attachments: cardAttachments } = author.kind === "self" || author.kind === "teammate" || author.kind === "unknown"
                  ? splitAttachmentsFooter(message.content)
                  : { body: message.content, attachments: [] as ParsedAttachment[] }
                return (
                  <div key={message.id} className={cn("flex w-full", author.kind === "self" ? "justify-end" : "justify-start")}>
                    <div className={cn(
                      "text-xs p-2 rounded",
                      author.kind === "self"
                        ? "bg-blue-600/[0.28] border border-blue-500/45"
                        : author.kind === "teammate" || author.kind === "unknown" ? "bg-secondary" : (claw.color && COLOR_CLASSES[claw.color]?.bubble) || "bg-secondary"
                    )}>
                    <div className="flex items-center gap-1 mb-0.5">
                      {author.kind === "agent" ? <Bot className="size-3 text-muted-foreground" /> : author.kind === "teammate" ? <span className="flex size-4 items-center justify-center rounded-full text-[8px] font-semibold text-background" style={{ backgroundColor: author.color }}>{author.initials}</span> : null}
                      <span className="font-medium text-foreground/70" style={author.kind === "teammate" ? { color: author.color } : undefined}>{author.kind === "self" ? "You" : author.kind === "teammate" ? author.name : author.kind === "unknown" ? "Teammate" : claw.name}</span>
                      {(author.kind === "teammate" || author.kind === "unknown") && <span className="text-[9px] text-muted-foreground">teammate</span>}
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
                  </div>
                )
              })
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

          {/* Footer stat line */}
          <div className="flex items-center gap-3 border-t border-border px-3 py-1 font-mono text-[10px] text-muted-foreground">
            <span>
              {cardStats.toolCalls} step{cardStats.toolCalls === 1 ? "" : "s"}
            </span>
            {cardStats.failures > 0 && (
              <span className="text-[var(--text-error)]">
                {cardStats.failures} failed
              </span>
            )}
            <span className="ml-auto">ctx {claw.contextUsage}%</span>
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

      </div>
    </div>
    <Sheet open={detailsOpen} onOpenChange={setDetailsOpen}>
      <SheetContent className="flex w-full flex-col sm:max-w-md">
        <SheetHeader><SheetTitle className="font-mono text-sm">{claw.name} — Agent details</SheetTitle></SheetHeader>
        <ClawCardBack claw={claw} open={detailsOpen} />
        <div className="flex gap-2 border-t border-border pt-3">
          <Button variant="destructive" size="sm" className="flex-1" onClick={() => setConfirmKill(true)}><Trash2 className="mr-1.5 size-3" />Kill</Button>
          <Button variant="outline" size="sm" className="flex-1" disabled={!claw.ssh_host} onClick={() => setShowTerminal(true)}><TerminalSquare className="mr-1.5 size-3" />Terminal</Button>
        </div>
      </SheetContent>
    </Sheet>
    <KillConfirmDialog clawName={claw.name} open={confirmKill} onConfirm={() => { setConfirmKill(false); onKill(claw.id) }} onCancel={() => setConfirmKill(false)} />
    {/* Terminal dialog stays outside the card so it can fill the viewport. */}
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
  currentUserLogin,
  currentUserResolved,
}: {
  claw: Claw
  messages: Message[]
  streamingBuffer?: TypewriterState
  onClick: (clawId: string) => void
  onSendMessage: (clawId: string, content: string) => void
  onKill: (clawId: string) => void
  currentUserLogin?: string | null
  currentUserResolved: boolean
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
        currentUserLogin={currentUserLogin}
        currentUserResolved={currentUserResolved}
        dragHandleProps={{ ...attributes, ...listeners }}
      />
    </div>
  )
})

function BoardSection({
  section,
  className,
  isMobile = false,
  children,
}: {
  section: { key: AgentSectionName; meta: typeof AGENT_SECTION[AgentSectionName]; items: Claw[] }
  className?: string
  isMobile?: boolean
  children: React.ReactNode
}) {
  const Icon = section.meta.icon
  return (
    <section className={cn("flex flex-col gap-3", !isMobile && "min-w-[500px]", className)}>
      <div className="flex items-center gap-2 rounded-md border px-3 py-2" style={{ backgroundColor: `color-mix(in srgb, ${section.meta.color} 10%, var(--card))`, borderColor: `color-mix(in srgb, ${section.meta.color} 30%, var(--border))` }}>
        <Icon className="size-[13px]" style={{ color: section.meta.color }} />
        <span className="text-xs font-semibold uppercase tracking-[0.08em]">{section.meta.label}</span>
        <span className="ml-auto rounded-full px-1.5 font-mono text-[10px]" style={{ backgroundColor: `color-mix(in srgb, ${section.meta.color} 22%, transparent)`, color: section.meta.color }}>{section.items.length}</span>
      </div>
      {children}
    </section>
  )
}

function formatTimestamp(date: Date): string {
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
}

const MessageBubble = memo(function MessageBubble({
  message,
  clawId,
  clawName,
  clawColor,
  currentUserLogin,
  currentUserResolved,
}: {
  message: Message
  clawId: string
  clawName: string
  clawColor?: string
  currentUserLogin?: string | null
  currentUserResolved: boolean
}) {
  if (message.role === "system") {
    if (message.content === "__TOOL_GAP__") {
      return (
        <div className="flex items-center gap-2 py-2">
          <div className="flex-1 h-px bg-border/50" />
          <div className="flex items-center gap-1.5 text-muted-foreground/50">
            <Wrench className="size-3" />
            <span className="text-[10px] uppercase tracking-[0.08em]">tool call</span>
          </div>
          <div className="flex-1 h-px bg-border/50" />
        </div>
      )
    }
    return (
      <div className="flex items-center gap-3 py-4">
        <div className="flex-1 h-px bg-border" />
        <span className="text-xs text-muted-foreground uppercase tracking-[0.08em] font-medium">
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

  const author = messageAuthor(message, currentUserLogin, currentUserResolved)
  const isUser = author.kind === "self" || author.kind === "teammate" || author.kind === "unknown"
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
    <div className={cn("flex w-full", author.kind === "self" ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "w-fit max-w-[88%] md:w-[70%] md:max-w-none min-w-0 rounded-lg px-4 py-3",
          author.kind === "self"
            ? "bg-blue-600/[0.28] border border-blue-500/45"
            : author.kind === "teammate" || author.kind === "unknown" ? "bg-secondary" : (clawColor && COLOR_CLASSES[clawColor]?.bubble) || "bg-secondary"
        )}
      >
        <div className="flex items-center gap-2 mb-1">
          {author.kind === "agent" ? <Bot className="size-4 text-muted-foreground" /> : author.kind === "teammate" ? <span className="flex size-5 items-center justify-center rounded-full text-[9px] font-semibold text-background" style={{ backgroundColor: author.color }}>{author.initials}</span> : null}
          <span className={cn("text-xs font-medium", author.kind === "self" ? "text-muted-foreground" : "text-foreground")} style={author.kind === "teammate" ? { color: author.color } : undefined}>{author.kind === "self" ? "You" : author.kind === "teammate" ? author.name : author.kind === "unknown" ? "Teammate" : clawName}</span>
          {(author.kind === "teammate" || author.kind === "unknown") && <span className="text-[10px] text-muted-foreground">teammate</span>}
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

/**
 * Panel / Lanes segmented pair. "off" is reachable by clicking the active
 * segment again — the pair reads as two choices, but a user who wants the
 * transcript full-width should not have to hunt for a third button.
 */
function SubagentViewToggle({
  view,
  onChange,
}: {
  view: SubagentView
  onChange: (v: SubagentView) => void
}) {
  const options: { value: Exclude<SubagentView, "off">; label: string }[] = [
    { value: "rail", label: "Panel" },
    { value: "lanes", label: "Lanes" },
  ]
  return (
    <div
      role="group"
      aria-label="Subagent view"
      className="flex items-center gap-0.5 rounded-md border border-border bg-muted/30 p-0.5"
    >
      {options.map((option) => {
        const active = view === option.value
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(active ? "off" : option.value)}
            title={active ? `Hide the subagent ${option.label.toLowerCase()}` : `Show subagents as ${option.label.toLowerCase()}`}
            className={cn(
              "rounded px-2 py-0.5 text-[10.5px] transition-colors",
              active
                ? "bg-accent font-medium text-accent-foreground"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}

// ─── ClawChatView ─────────────────────────────────────────────────────────────
// Extracted so scroll refs are only live when this branch is mounted.

function ClawChatView({
  claw,
  messages: liveMessages,
  streamingBuffer,
  onSendMessage,
  onKill,
  onDeselectClaw,
  currentUserLogin,
  currentUserResolved,
}: {
  claw: Claw
  messages: Message[]
  streamingBuffer?: TypewriterState
  onSendMessage: (content: string) => void
  onKill: () => void
  onDeselectClaw: () => void
  currentUserLogin?: string | null
  currentUserResolved: boolean
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
    unloadedActivityCount,
    loadingActivity,
    loadAllActivity,
  } = useWindowedMessages({
    clawId: claw.id,
    liveMessages,
  })
  const [density, setDensity] = useTimelineDensity()
  // An offline/errored claw cannot still be running its dangling last step.
  const allowTrailingRunning = claw.status !== "offline" && claw.status !== "error"
  const [turnsState, setTurnsState] = useState(() => ({
    messages,
    allowTrailingRunning,
    turns: groupIntoTurns(messages, allowTrailingRunning),
  }))
  let turns = turnsState.turns
  if (turnsState.messages !== messages || turnsState.allowTrailingRunning !== allowTrailingRunning) {
    turns = groupIntoTurns(messages, allowTrailingRunning, turnsState.turns)
    setTurnsState({ messages, allowTrailingRunning, turns })
  }
  const stats = useMemo(() => timelineStats(turns), [turns])
  const runningStep = useMemo(() => latestRunningStep(turns), [turns])
  const isWorking = claw.isStreaming || Boolean(runningStep)

  // Subagents. The clock only ticks while the claw is working: a finished
  // transcript has no running subagent whose staleness could change, so an
  // idle chat must not re-derive this list once a second.
  const subagentNow = useNowTick(isWorking)
  const subagents = useMemo(() => collectSubagents(turns, subagentNow), [turns, subagentNow])
  const [subagentView, setSubagentView] = useSubagentView()
  // Mobile has no room for a 300px rail or a lane strip above a phone-height
  // transcript — Task step rows remain the way in there.
  const effectiveSubagentView: SubagentView = isMobile ? "off" : subagentView
  // Reset-on-prop-change during render (the same shape as turnsState above)
  // rather than an effect: selecting another claw must not leave a stale
  // drill-down mounted, and an effect would paint the wrong claw's subagent
  // for one frame first.
  const [openSubagentState, setOpenSubagentState] = useState<{ clawId: string; id: string | null }>({
    clawId: claw.id,
    id: null,
  })
  if (openSubagentState.clawId !== claw.id) setOpenSubagentState({ clawId: claw.id, id: null })
  const openSubagentId = openSubagentState.clawId === claw.id ? openSubagentState.id : null
  const setOpenSubagentId = useCallback(
    (id: string | null) => setOpenSubagentState({ clawId: claw.id, id }),
    [claw.id]
  )
  const openSubagent = openSubagentId
    ? subagents.find((sub) => sub.id === openSubagentId) ?? null
    : null

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

  // Opening a drill-down replaces the transcript body, so the reading position
  // must go to the top of the new content and the bottom-pin must let go —
  // otherwise usePinnedAutoScroll would immediately drag the user to the end
  // of the subagent's result.
  const handleOpenSubagent = useCallback(
    (id: string) => {
      setOpenSubagentId(id)
      pinnedToBottom.current = false
      setShowScrollBtn(false)
      const el = scrollRef.current
      if (el) {
        markProgrammaticScroll()
        el.scrollTop = 0
      }
    },
    [scrollRef, markProgrammaticScroll, setOpenSubagentId]
  )

  const handleCloseSubagent = useCallback(() => {
    setOpenSubagentId(null)
    const el = scrollRef.current
    if (el) {
      markProgrammaticScroll()
      el.scrollTop = el.scrollHeight
      pinnedToBottom.current = true
    }
  }, [scrollRef, markProgrammaticScroll, setOpenSubagentId])

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
        currentUserLogin={currentUserLogin}
        currentUserResolved={currentUserResolved}
      />
    ),
    [claw.id, claw.name, claw.color, currentUserLogin, currentUserResolved]
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
            <div className="min-w-0 flex-1 overflow-hidden">
              <ClawTitle
                name={claw.name}
                githubIssueId={claw.githubIssueId}
                githubIssueUrl={claw.githubIssueUrl}
                className="block font-mono text-base font-semibold text-foreground"
              />
            </div>
            {/* Uptime is intentionally dropped here: at 320-375px it does not
                fit next to the badge and the menu (it stays visible on the
                board card and desktop header). The badge never shrinks. */}
            <StatusBadge status={claw.status} className="shrink-0" />
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
              {/* Only offered when there is something to show — an agent that
                  never spawned a subagent looks exactly as it did before. */}
              {subagents.length > 0 && (
                <SubagentViewToggle view={subagentView} onChange={setSubagentView} />
              )}
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
      {effectiveSubagentView === "lanes" && !openSubagent && (
        <SubagentLanes subagents={subagents} now={subagentNow} onOpen={handleOpenSubagent} />
      )}

      {/* The row wrapper owns the remaining height; scrollRef stays the one
          scrolling element so the pin/anchor machinery is untouched. */}
      <div className="flex min-h-0 flex-1">
      <div ref={scrollRef} onScroll={handleScroll} className="min-w-0 flex-1 overflow-y-auto scrollbar-thin p-4 md:p-6 relative">
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
          {openSubagent ? (
            <>
              <button
                type="button"
                onClick={handleCloseSubagent}
                className="flex items-center gap-1 rounded-sm py-0.5 pr-2 text-[11px] text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring"
              >
                <ChevronLeft className="size-3.5 shrink-0" />
                <span className="min-w-0 truncate font-mono">{claw.name}</span>
              </button>
              <SubagentDetail subagent={openSubagent} now={subagentNow} />
            </>
          ) : messages.length === 0 && !streamingBuffer ? (
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
              unloadedToolCalls={unloadedActivityCount}
              loadingUnloaded={loadingActivity}
              onLoadUnloaded={loadAllActivity}
              onOpenSubagent={subagents.length > 0 ? handleOpenSubagent : undefined}
            />
          )}
          <div ref={bottomRef} className="h-4" />
        </div>
        {showScrollBtn && !openSubagent && (
          <button
            onClick={scrollToBottom}
            className="absolute bottom-4 left-1/2 -translate-x-1/2 z-10 flex items-center gap-1.5 px-3 py-1.5 rounded-full bg-secondary border border-border text-xs text-muted-foreground hover:text-foreground hover:bg-accent transition-colors shadow-md"
          >
            <ChevronDown className="size-3.5" />
            <span>Scroll to bottom</span>
          </button>
        )}
      </div>
        {effectiveSubagentView === "rail" && subagents.length > 0 && (
          <SubagentRail subagents={subagents} now={subagentNow} onOpen={handleOpenSubagent} />
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
  currentUserLogin,
  currentUserResolved,
}: ConversationViewProps) {
  const boardRef = useRef<HTMLDivElement>(null)
  const [activeDragClaw, setActiveDragClaw] = useState<Claw | null>(null)
  const { logoUrl } = useBranding()
  const isMobile = useIsMobile()
  const handleCardClick = useCallback((clawId: string) => onSelectClaw(clawId), [onSelectClaw])
  const handleCardSendMessage = useCallback((clawId: string, content: string) => onSendMessageToClaw(clawId, content), [onSendMessageToClaw])
  const handleCardKill = useCallback((clawId: string) => onKillClaw(clawId), [onKillClaw])
  const calculatedBoardSectionIds = useMemo(() => {
    const itemsBySection = new Map<AgentSectionName, Claw[]>([
      ["attention", []], ["working", []], ["offline", []],
    ])
    for (const candidate of allClaws) {
      const section = agentSection(candidate, { isWaitingOnYou: isWaitingOnYou(allMessages[candidate.id] ?? EMPTY_MESSAGES) })
      itemsBySection.get(section)!.push(candidate)
    }
    return (["attention", "working", "offline"] as AgentSectionName[]).map((key) => itemsBySection.get(key)!.sort((a, b) => Number(b.pinned) - Number(a.pinned)).map((candidate) => candidate.id))
  }, [allClaws, allMessages])
  const [boardSectionIds, setBoardSectionIds] = useState(calculatedBoardSectionIds)
  if (boardSectionIds.some((ids, index) => ids.length !== calculatedBoardSectionIds[index].length || ids.some((id, idIndex) => id !== calculatedBoardSectionIds[index][idIndex]))) {
    setBoardSectionIds(calculatedBoardSectionIds)
  }
  const boardSections = useMemo(() => {
    const clawsById = new Map(allClaws.map((candidate) => [candidate.id, candidate]))
    return (["attention", "working", "offline"] as AgentSectionName[]).map((key, index) => {
      const ids = boardSectionIds[index]
      const items = ids.map((id) => clawsById.get(id)).filter((candidate): candidate is Claw => candidate !== undefined)
      return { key, meta: AGENT_SECTION[key], items, ids }
    })
  }, [allClaws, boardSectionIds])

  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: { distance: 6 },
    })
  )

  function handleBoardDragStart(event: DragStartEvent) {
    const found = allClaws.find((c) => c.id === event.active.id)
    setActiveDragClaw(found ?? null)
  }

  const collisionDetectionForClaws: CollisionDetection = useCallback((args) => {
    const activeClaw = allClaws.find((candidate) => candidate.id === args.active.id)
    if (!activeClaw) return closestCenter(args)
    const sectionFor = (candidate: Claw) => boardSections.find((section) => section.items.includes(candidate))?.key
    const activeSection = sectionFor(activeClaw)
    const allowed = args.droppableContainers.filter((container) => {
      const overClaw = allClaws.find((candidate) => candidate.id === container.id)
      return container.id === args.active.id || Boolean(overClaw && sectionFor(overClaw) === activeSection && overClaw.pinned === activeClaw.pinned)
    })
    return closestCenter({ ...args, droppableContainers: allowed })
  }, [allClaws, boardSections])

  function handleBoardDragEnd(event: DragEndEvent) {
    setActiveDragClaw(null)
    const { active, over } = event
    if (!over || active.id === over.id) return
    const activeClaw = allClaws.find((candidate) => candidate.id === active.id)
    const overClaw = allClaws.find((candidate) => candidate.id === over.id)
    if (!activeClaw || !overClaw) return
    const sectionFor = (candidate: Claw) => boardSections.find((section) => section.items.includes(candidate))?.key
    if (sectionFor(activeClaw) !== sectionFor(overClaw)) return
    if (activeClaw.pinned !== overClaw.pinned) return
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
      // One card plus the flex gap, so the arrows page card-by-card.
      const scrollAmount = 516
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
    const sectionSummary = boardSections.filter((section) => section.items.length > 0)
      .map((section) => `${section.items.length} ${section.meta.label.toLowerCase()}`).join(" · ")

    return (
      <main className="flex-1 flex flex-col bg-background min-w-0 overflow-hidden">
        {/* Header */}
        {/* Kit BoardScreen header: one row — glyph, "Agents", the section
            summary inline in mono, and the overflow menu on the right edge.
            The old status legend is gone: the section bands carry that
            vocabulary now. */}
        <header className="flex shrink-0 items-center gap-2 border-b border-border px-3 py-2 md:px-4 md:py-2.5">
          {isMobile && onOpenMenu ? (
            <Button variant="ghost" size="icon" className="size-11 shrink-0" onClick={onOpenMenu} title="Open agent list">
              <Menu className="size-5" />
            </Button>
          ) : (
            <LayoutGrid className="size-4 shrink-0 text-muted-foreground" />
          )}
          <span className="shrink-0 text-sm font-medium">Agents</span>
          <span className="min-w-0 truncate font-mono text-[10.5px] text-muted-foreground">{loading ? "Loading agents" : sectionSummary}</span>
          <div className="ml-auto flex min-w-0 items-center gap-2">
            <DependencyDowntimeBanner dependencies={downtimeDependencies} />
            {/* Sign out lives here on mobile — the tab bar has no room for it */}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size={isMobile ? "icon" : "icon-sm"} className={isMobile ? "size-11 shrink-0" : "shrink-0"} title="More" aria-label="More">
                  <MoreVertical className={isMobile ? "size-5" : "size-4"} />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={() => { void signOut() }}>
                  <LogOut className="size-4" />
                  Sign out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </header>

        {/* Board view */}
        <div className="flex-1 relative min-h-0">
          {allClaws.length === 0 && !loading ? (
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
            <div className="h-full overflow-y-auto overflow-x-hidden p-3 space-y-5">
              {boardSections.filter((section) => section.items.length).map((section) => <BoardSection key={section.key} section={section} isMobile><div className="flex flex-col gap-3">{section.items.map((c) => <ClawBoardCard key={c.id} claw={c} messages={allMessages[c.id] ?? EMPTY_MESSAGES} streamingBuffer={streamingBuffers[c.id]} onClick={handleCardClick} onSendMessage={handleCardSendMessage} onKill={handleCardKill} currentUserLogin={currentUserLogin} currentUserResolved={currentUserResolved} />)}</div></BoardSection>)}
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
            collisionDetection={collisionDetectionForClaws}
            onDragStart={handleBoardDragStart}
            onDragEnd={handleBoardDragEnd}
          >
              <div
                ref={boardRef}
                className="flex gap-6 h-full overflow-x-auto overflow-y-hidden p-3 items-stretch"
                style={{ scrollbarWidth: "none", msOverflowStyle: "none" }}
              >
                {boardSections.filter((section) => section.items.length).map((section) => <BoardSection key={section.key} section={section} className="h-full shrink-0"><SortableContext items={section.ids} strategy={horizontalListSortingStrategy}><div className="flex flex-1 min-h-0 gap-3">{section.items.map((c) => <SortableClawBoardCard key={c.id} claw={c} messages={allMessages[c.id] ?? EMPTY_MESSAGES} streamingBuffer={streamingBuffers[c.id]} onClick={handleCardClick} onSendMessage={handleCardSendMessage} onKill={handleCardKill} currentUserLogin={currentUserLogin} currentUserResolved={currentUserResolved} />)}</div></SortableContext></BoardSection>)}
              </div>

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
                    currentUserLogin={currentUserLogin}
                    currentUserResolved={currentUserResolved}
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
      currentUserLogin={currentUserLogin}
      currentUserResolved={currentUserResolved}
    />
  )
}
