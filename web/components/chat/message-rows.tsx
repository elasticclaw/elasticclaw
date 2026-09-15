"use client"

import { memo, useCallback, useEffect, useRef, useState } from "react"
import { Brain, Check, Copy, Settings2, Wrench, type LucideIcon } from "lucide-react"
import { MarkdownContent } from "@/components/markdown-content"
import { AttachmentChip } from "@/components/attachment-chip"
import { StepRow } from "@/components/agent-timeline/step-row"
import { demoteStaleRunning, pairActivitySteps } from "@/lib/turns"
import { splitAttachmentsFooter, type ParsedAttachment } from "@/lib/attachments"
import { messageAuthor, type MessageAuthor } from "@/lib/message-author"
import { copyTextToClipboard } from "@/lib/transcript"
import type { Message } from "@/lib/types"
import type { TypewriterState } from "@/hooks/use-typewriter"
import { cn } from "@/lib/utils"

/** "chat" is the full transcript column; "card" is the board card at text-xs. */
export type RowVariant = "chat" | "card"

export function formatTimestamp(date: Date): string {
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
}

// ─── small rows ───────────────────────────────────────────────────────────────

/** Hairline divider with a centered label (system notices, tool gaps, state). */
export function SeparatorRow({
  label,
  detail,
  icon: Icon,
  variant = "chat",
}: {
  label: string
  detail?: string
  icon?: LucideIcon
  variant?: RowVariant
}) {
  return (
    <div
      role="separator"
      className={cn(
        "flex items-center gap-3 py-1 text-muted-foreground pb-1.5",
        variant === "chat" ? "text-xs" : "text-[10px]"
      )}
    >
      <span className="h-px flex-1 bg-border/70" />
      <span className="flex items-center gap-1.5">
        {Icon && <Icon className="size-3" />}
        <span>{label}</span>
        {detail && <span className="text-foreground/70">{detail}</span>}
      </span>
      <span className="h-px flex-1 bg-border/70" />
    </div>
  )
}

export function ToolGapRow({ variant = "chat" }: { variant?: RowVariant }) {
  return <SeparatorRow label="Tool call" icon={Wrench} variant={variant} />
}

export function HubNoticeRow({ message, variant = "chat" }: { message: Message; variant?: RowVariant }) {
  return (
    <div
      className={cn(
        "flex items-start gap-1.5 rounded-md px-1 py-0.5 text-muted-foreground pb-1",
        variant === "chat" ? "text-xs" : "text-[10px]"
      )}
    >
      <span className={cn("flex shrink-0 items-center justify-center text-icon-muted", variant === "chat" ? "size-6" : "size-4")}>
        <Settings2 className={variant === "chat" ? "size-3.5" : "size-3"} />
      </span>
      <span className={cn("min-w-0 leading-relaxed", message.format === "pre" && "whitespace-pre-wrap")}>
        {message.content}
      </span>
    </div>
  )
}

export function ThinkingRow({ variant = "chat" }: { variant?: RowVariant }) {
  return (
    <div
      className={cn(
        "flex items-center gap-1.5 px-1 text-secondary-label pb-1.5",
        variant === "chat" ? "min-h-7 text-sm" : "min-h-5 text-xs"
      )}
      aria-live="polite"
    >
      <span className={cn("flex shrink-0 items-center justify-center text-icon-muted", variant === "chat" ? "size-6" : "size-4")}>
        <Brain className={cn("stroke-[1.8]", variant === "chat" ? "size-4" : "size-3.5")} />
      </span>
      <span className="live-tool-shine">Thinking</span>
    </div>
  )
}

// ─── copy one message ─────────────────────────────────────────────────────────

function MessageCopyButton({ text, className }: { text: string; className?: string }) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<number | null>(null)
  useEffect(() => () => { if (timer.current) window.clearTimeout(timer.current) }, [])
  const copy = useCallback(async (e: React.MouseEvent) => {
    e.stopPropagation()
    if (!(await copyTextToClipboard(text))) return
    setCopied(true)
    if (timer.current) window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setCopied(false), 2000)
  }, [text])
  return (
    <button
      type="button"
      onClick={copy}
      aria-label={copied ? "Copied" : "Copy message"}
      title={copied ? "Copied" : "Copy message"}
      className={cn(
        "flex size-6 items-center justify-center rounded-[var(--control-radius)] text-muted-foreground transition-colors hover:bg-accent/40 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/70",
        className
      )}
    >
      {copied ? <Check className="size-3 text-primary" /> : <Copy className="size-3" />}
    </button>
  )
}

// ─── user / teammate bubble ───────────────────────────────────────────────────

const COLLAPSE_CHARS = 600
const COLLAPSE_LINES = 8
const COLLAPSED_FADE_MASK = "linear-gradient(to bottom, black calc(100% - 1.75rem), transparent)"

function shouldCollapse(text: string): boolean {
  return text.length > COLLAPSE_CHARS || text.split("\n").length > COLLAPSE_LINES
}

function CollapsibleBody({ text, variant }: { text: string; variant: RowVariant }) {
  const [expanded, setExpanded] = useState(false)
  const collapsible = variant === "chat" && shouldCollapse(text)
  const collapsed = collapsible && !expanded
  return (
    <div>
      <p
        className={cn(
          "whitespace-pre-wrap leading-relaxed",
          variant === "chat" ? "text-sm" : "text-xs",
          collapsed && "max-h-44 overflow-hidden"
        )}
        style={collapsed ? { WebkitMaskImage: COLLAPSED_FADE_MASK, maskImage: COLLAPSED_FADE_MASK } : undefined}
      >
        {text}
      </p>
      {collapsible && (
        <div className="mt-1.5 flex justify-end">
          <button
            type="button"
            aria-expanded={expanded}
            onClick={(e) => { e.stopPropagation(); setExpanded((value) => !value) }}
            className="-ml-1 h-6 rounded-md px-1.5 text-xs text-secondary-label transition-colors hover:bg-muted/55 hover:text-message-foreground"
          >
            {expanded ? "Show less" : "Show full message"}
          </button>
        </div>
      )}
    </div>
  )
}

function UserAttachments({
  attachments,
  clawId,
  variant,
}: {
  attachments: ParsedAttachment[]
  clawId: string
  variant: RowVariant
}) {
  const images = attachments.filter((a) => a.mimetype.startsWith("image/"))
  const files = attachments.filter((a) => !a.mimetype.startsWith("image/"))
  const size = variant === "chat" ? "md" : "sm"
  const chip = (a: ParsedAttachment, i: number) => (
    <AttachmentChip
      key={`${a.path}-${i}`}
      name={a.name}
      sizeLabel={a.sizeLabel}
      mimetype={a.mimetype}
      source={{ kind: "history", clawId, path: a.path }}
      size={size}
      path={a.path}
    />
  )
  return (
    <>
      {images.length > 0 && (
        <div className={cn("mb-2 grid grid-cols-2 gap-2", variant === "chat" ? "max-w-[210px]" : "max-w-[160px]")}>
          {images.map(chip)}
        </div>
      )}
      {files.length > 0 && <div className="mb-2 flex flex-col gap-1">{files.map(chip)}</div>}
    </>
  )
}

function UserRow({
  message,
  author,
  clawId,
  variant,
}: {
  message: Message
  author: MessageAuthor
  clawId: string
  variant: RowVariant
}) {
  const self = author.kind === "self"
  const { body, attachments } = splitAttachmentsFooter(message.content)
  const name = author.kind === "teammate" ? author.name : "Teammate"
  return (
    <div className={cn("group flex flex-col gap-1", self ? "items-end pb-4" : "items-start pb-4", variant === "card" && "pb-2")}>
      {!self && (
        <div className={cn("flex items-center gap-1.5 px-1 text-muted-foreground", variant === "chat" ? "text-xs" : "text-[10px]")}>
          {author.kind === "teammate" && (
            <span
              className="flex size-4 items-center justify-center rounded-full text-[9px] font-semibold text-background"
              style={{ backgroundColor: author.color }}
            >
              {author.initials}
            </span>
          )}
          <span>{name}</span>
        </div>
      )}
      <div
        className={cn(
          "relative min-w-0 text-message-foreground",
          self ? "bg-message" : "bg-secondary",
          variant === "chat" ? "max-w-[80%] rounded-2xl p-3" : "max-w-[85%] rounded-xl px-2.5 py-2"
        )}
      >
        <h3 className="sr-only">{self ? "You" : name}</h3>
        {attachments.length > 0 && <UserAttachments attachments={attachments} clawId={clawId} variant={variant} />}
        {body.trim() && <CollapsibleBody text={body} variant={variant} />}
      </div>
      <div
        className={cn(
          "flex w-full items-center gap-1 pe-1 tabular-nums text-muted-foreground opacity-0 transition-opacity duration-200 group-hover:opacity-100 focus-within:opacity-100 pointer-coarse:opacity-100",
          self ? "justify-end" : "justify-start",
          variant === "chat" ? "max-w-[80%] text-xs" : "max-w-[85%] text-[10px]"
        )}
      >
        <span suppressHydrationWarning>{formatTimestamp(message.timestamp)}</span>
        {body.trim() && <MessageCopyButton text={body} />}
      </div>
    </div>
  )
}

// ─── assistant row ────────────────────────────────────────────────────────────

function AssistantRow({
  text,
  clawName,
  timestamp,
  variant,
  streaming,
}: {
  text: string
  clawName: string
  timestamp: Date
  variant: RowVariant
  streaming?: boolean
}) {
  return (
    <div className={cn("group/assistant relative min-w-0 px-1 py-0.5", variant === "chat" ? "pb-2" : "pb-1.5")}>
      <h3 className="sr-only">{clawName}</h3>
      <MarkdownContent
        content={text}
        streaming={streaming}
        className={cn("leading-relaxed text-foreground/80", variant === "chat" ? "text-sm" : "text-xs")}
      />
      <div
        className={cn(
          "mt-1.5 flex items-center gap-2 tabular-nums text-muted-foreground opacity-0 transition-opacity duration-200 group-hover/assistant:opacity-100 focus-within:opacity-100 pointer-coarse:opacity-100",
          variant === "chat" ? "text-xs" : "text-[10px]"
        )}
      >
        {!streaming && <MessageCopyButton text={text} />}
        <span suppressHydrationWarning>{formatTimestamp(timestamp)}</span>
      </div>
    </div>
  )
}

// ─── conversation message (user or claw) ──────────────────────────────────────

export function ConversationMessage({
  message,
  clawId,
  clawName,
  variant,
  currentUserLogin,
  currentUserResolved,
}: {
  message: Message
  clawId: string
  clawName: string
  variant: RowVariant
  currentUserLogin?: string | null
  currentUserResolved: boolean
}) {
  const author = messageAuthor(message, currentUserLogin, currentUserResolved)
  if (author.kind === "agent") {
    return <AssistantRow text={message.content} clawName={clawName} timestamp={message.timestamp} variant={variant} />
  }
  return <UserRow message={message} author={author} clawId={clawId} variant={variant} />
}

// ─── MessageBubble: every chat transcript row ────────────────────────────────

export const MessageBubble = memo(function MessageBubble({
  message,
  clawId,
  clawName,
  currentUserLogin,
  currentUserResolved,
}: {
  message: Message
  clawId: string
  clawName: string
  currentUserLogin?: string | null
  currentUserResolved: boolean
}) {
  if (message.role === "system") {
    if (message.content === "__TOOL_GAP__") return <ToolGapRow />
    return <SeparatorRow label={message.content} />
  }

  if (message.role === "activity") {
    // Activities normally render inside turn cards; this is a defensive
    // fallback for stray rows reaching the bubble path.
    const step = demoteStaleRunning(pairActivitySteps([message]), false)[0]
    return step ? <StepRow step={step} /> : null
  }

  if (message.role === "state") return <SeparatorRow label="State" detail={message.content} />
  if (message.content === "__THINKING__") return <ThinkingRow />
  if (message.role === "hub") return <HubNoticeRow message={message} />

  return (
    <ConversationMessage
      message={message}
      clawId={clawId}
      clawName={clawName}
      variant="chat"
      currentUserLogin={currentUserLogin}
      currentUserResolved={currentUserResolved}
    />
  )
})

// ─── StreamingMessage ─────────────────────────────────────────────────────────

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
// Layout mirrors AssistantRow so the hand-off to the finalized message is invisible.
export function StreamingMessage({
  state,
  variant,
  clawName,
}: {
  state: TypewriterState
  variant: RowVariant
  clawName: string
}) {
  // Streaming messages have no server timestamp yet; freeze the start time so the
  // meta row does not change while the typewriter drains.
  const [startedAt] = useState(() => new Date())
  const text = useThrottledText(state.text)

  if (!state.hadChunks) return <ThinkingRow variant={variant} />
  if (!state.text) return null

  return <AssistantRow text={text} clawName={clawName} timestamp={startedAt} variant={variant} streaming />
}
