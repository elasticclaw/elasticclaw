"use client"

import { useState, useRef, useEffect, useCallback } from "react"
import { Send, Terminal, TerminalSquare, ChevronLeft, ChevronRight, ChevronDown, Loader2, LayoutGrid, Info, MessageSquare, RotateCcw, Trash2, AlertCircle, Wrench } from "lucide-react"
import { MarkdownContent } from "@/components/markdown-content"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"
import type { Claw, Message, ClawStatus } from "@/lib/types"
import { getTerminalWsUrl } from "@/lib/api"
import dynamic from "next/dynamic"

const XTerminal = dynamic(
  () => import("@/components/terminal").then((m) => m.XTerminal),
  { ssr: false }
)

interface ConversationViewProps {
  loading?: boolean
  hubError?: string | null
  claw: Claw | null
  allClaws: Claw[]
  messages: Message[]
  allMessages: Record<string, Message[]>
  onSendMessage: (content: string) => void
  onSendMessageToClaw: (clawId: string, content: string) => void
  onKill: () => void
  onKillClaw: (clawId: string) => void
  onNewSession: () => void
  onNewSessionForClaw: (clawId: string) => void
  onSelectClaw: (id: string) => void
  onDeselectClaw: () => void
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

function ClawBoardCard({ 
  claw, 
  messages,
  onClick,
  onSendMessage,
  onNewSession,
  onKill,
}: { 
  claw: Claw
  messages: Message[]
  onClick: () => void
  onSendMessage: (content: string) => void
  onNewSession: () => void
  onKill: () => void
}) {
  const [input, setInput] = useState("")
  const [isFlipped, setIsFlipped] = useState(false)
  const [showTerminal, setShowTerminal] = useState(false)
  const hasUnread = claw.unreadCount > 0
  const isPending = claw.status === "provisioning" || claw.status === "error" || claw.status === "offline"
  const msgScrollRef = useRef<HTMLDivElement>(null)
  const [showCardScrollBtn, setShowCardScrollBtn] = useState(false)

  useEffect(() => {
    const el = msgScrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [messages])

  const handleCardScroll = useCallback(() => {
    const el = msgScrollRef.current
    if (!el) return
    setShowCardScrollBtn(el.scrollHeight - el.scrollTop - el.clientHeight > 60)
  }, [])
  
  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    e.stopPropagation()
    if (input.trim()) {
      onSendMessage(input.trim())
      setInput("")
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
        "w-[320px] h-full shrink-0 relative",
        "[perspective:1000px]"
      )}
    >
      <div
        className={cn(
          "relative w-full h-full transition-transform duration-500",
          "[transform-style:preserve-3d]",
          isFlipped && "[transform:rotateY(180deg)]"
        )}
      >
        {/* Front - Chat view */}
        <div
          className={cn(
            "absolute inset-0 flex flex-col rounded-lg border border-border bg-card",
            "[backface-visibility:hidden]",
            hasUnread && "border-blue-500/30 bg-blue-950/10",
            isPending && "opacity-75"
          )}
        >
          {claw.isStreaming && (
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
              <StatusDot status={claw.status} isStreaming={claw.isStreaming} />
              <button
                onClick={isPending ? undefined : onClick}
                className={cn(
                  "font-mono text-sm font-medium text-foreground truncate flex-1 text-left",
                  !isPending && "hover:underline"
                )}
              >
                {claw.name}
              </button>
              {hasUnread && (
                <span className="px-1.5 py-0.5 text-[10px] font-medium bg-blue-500 text-white rounded-full">
                  {claw.unreadCount > 99 ? "99+" : claw.unreadCount}
                </span>
              )}
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
            {Object.keys(claw.tags).length > 0 && (
              <div className="flex flex-wrap gap-1 mt-2">
                {Object.entries(claw.tags).slice(0, 3).map(([key, value]) => (
                  <span
                    key={key}
                    className="inline-flex items-center px-1.5 py-0.5 text-[10px] font-medium bg-secondary text-muted-foreground rounded"
                  >
                    <span className="text-foreground/70">{key}</span>
                    <span className="mx-0.5">=</span>
                    <span>{value}</span>
                  </span>
                ))}
                {Object.keys(claw.tags).length > 3 && (
                  <span className="text-[10px] text-muted-foreground">
                    +{Object.keys(claw.tags).length - 3}
                  </span>
                )}
              </div>
            )}
          </div>
          
          {/* Messages area */}
          <div className="flex-1 relative min-h-0 overflow-hidden">
          <div ref={msgScrollRef} onScroll={handleCardScroll} className="h-full overflow-y-auto p-3 space-y-2">
            {messages.length === 0 ? (
              <p className="text-xs text-muted-foreground text-center py-4">
                No messages yet
              </p>
            ) : (
              messages.map((message) => {
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
                    <MarkdownContent content={message.content} className="text-xs text-foreground" />
                  </div>
                )
              })
            )}
          </div>
          {showCardScrollBtn && (
            <button
              onClick={() => { const el = msgScrollRef.current; if (el) el.scrollTop = el.scrollHeight }}
              className="absolute bottom-1 left-1/2 -translate-x-1/2 z-10 size-6 rounded-full bg-secondary border border-border flex items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent transition-colors shadow-sm"
            >
              <ChevronDown className="size-3.5" />
            </button>
          )}
          </div>
          
          {/* Input area */}
          <form onSubmit={isPending ? (e) => e.preventDefault() : handleSubmit} className="p-2 border-t border-border">
            <div className="flex gap-1.5">
              <Input
                value={input}
                onChange={(e) => setInput(e.target.value)}
                placeholder={isPending ? (claw.status === "error" ? "Provisioning failed" : claw.status === "offline" ? "Claw offline" : "Starting up...") : "Send message..."}
                className="h-8 text-xs flex-1"
                disabled={isPending}
                onClick={(e) => e.stopPropagation()}
              />
              <Button 
                type="submit" 
                size="icon" 
                className="size-8 shrink-0"
                disabled={!input.trim() || isPending}
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
            "[backface-visibility:hidden] [transform:rotateY(180deg)]"
          )}
        >
          {/* Header */}
          <div className="p-3 border-b border-border">
            <div className="flex items-center gap-2">
              <StatusDot status={claw.status} isStreaming={claw.isStreaming} />
              <span className="font-mono text-sm font-medium text-foreground truncate flex-1">
                {claw.name}
              </span>
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
          <div className="flex-1 overflow-y-auto p-4">
            <div className="space-y-4">
              <div>
                <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
                  Purpose
                </h3>
                <p className="text-sm text-foreground leading-relaxed">
                  {claw.description || "No description provided for this claw."}
                </p>
              </div>

              <div>
                <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
                  Template
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

              {Object.keys(claw.tags).length > 0 && (
                <div>
                  <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider mb-2">
                    Tags
                  </h3>
                  <div className="flex flex-wrap gap-1.5">
                    {Object.entries(claw.tags).map(([key, value]) => (
                      <span
                        key={key}
                        className="inline-flex items-center px-2 py-1 text-xs font-medium bg-secondary text-muted-foreground rounded"
                      >
                        <span className="text-foreground/70">{key}</span>
                        <span className="mx-1">=</span>
                        <span>{value}</span>
                      </span>
                    ))}
                  </div>
                </div>
              )}
            </div>
          </div>

          {/* Footer */}
          <div className="p-3 border-t border-border space-y-2">
            <Button 
              variant="outline" 
              size="sm" 
              className="w-full"
              onClick={onClick}
            >
              Open Full View
            </Button>
            <div className="flex gap-2">
              <Button 
                variant="outline" 
                size="sm" 
                className="flex-1"
                onClick={(e) => {
                  e.stopPropagation()
                  onNewSession()
                }}
              >
                <RotateCcw className="size-3 mr-1.5" />
                New Session
              </Button>
              <Button 
                variant="destructive" 
                size="sm" 
                className="flex-1"
                onClick={(e) => {
                  e.stopPropagation()
                  onKill()
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
}

function formatTimestamp(date: Date): string {
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
}

function MessageBubble({
  message,
  clawName,
}: {
  message: Message
  clawName: string
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

  return (
    <div className={cn("flex w-full", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[70%] min-w-0 rounded-lg px-4 py-3",
          isUser
            ? "bg-blue-600/20 border border-blue-500/20"
            : "bg-secondary"
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
        {isUser ? (
          <p className="text-sm whitespace-pre-wrap text-foreground">{message.content}</p>
        ) : (
          <MarkdownContent content={message.content} className="text-sm" />
        )}
      </div>
    </div>
  )
}

// ─── ClawChatView ─────────────────────────────────────────────────────────────
// Extracted so scroll refs are only live when this branch is mounted.

function ClawChatView({
  claw,
  messages,
  onSendMessage,
  onKill,
  onNewSession,
  onDeselectClaw,
}: {
  claw: Claw
  messages: Message[]
  onSendMessage: (content: string) => void
  onKill: () => void
  onNewSession: () => void
  onDeselectClaw: () => void
}) {
  const [input, setInput] = useState("")
  const [terminalOpen, setTerminalOpen] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const bottomRef = useRef<HTMLDivElement>(null)
  const [showScrollBtn, setShowScrollBtn] = useState(false)

  const isAtBottom = useCallback(() => {
    const el = scrollRef.current
    if (!el) return true
    return el.scrollHeight - el.scrollTop - el.clientHeight < 200
  }, [])

  const scrollToBottom = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
  }, [])

  const handleScroll = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    setShowScrollBtn(el.scrollHeight - el.scrollTop - el.clientHeight > 120)
  }, [])

  // On mount and when messages change: scroll to bottom
  useEffect(() => {
    const run = () => {
      const el = scrollRef.current
      if (el) el.scrollTop = el.scrollHeight
    }
    const timers = [0, 50, 150, 400, 800].map((d) => setTimeout(run, d))
    return () => timers.forEach(clearTimeout)
  }, [messages])

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (input.trim()) {
      onSendMessage(input)
      setInput("")
    }
  }

  return (
    <main className="flex-1 flex flex-col bg-background min-h-0 overflow-hidden">
      <header className="border-b border-border">
        <div className="px-6 pt-2">
          <ContextProgressBar usage={claw.contextUsage} size="lg" />
        </div>
        <div className="flex items-center justify-between px-6 py-3">
          <div className="flex items-center gap-4">
            <Button variant="ghost" size="icon" onClick={onDeselectClaw} title="Back to dashboard" className="size-8">
              <LayoutGrid className="size-4" />
            </Button>
            <h2 className="font-mono text-xl font-semibold text-foreground">{claw.name}</h2>
            <StatusBadge status={claw.status} />
            <span className="text-sm text-muted-foreground">{claw.template}</span>
            <span className="text-sm text-muted-foreground font-mono">{formatUptime(claw.uptime)}</span>
          </div>
          <div className="flex items-center gap-2">
            {claw.ssh_host && (
              <Button variant="outline" size="sm" onClick={() => setTerminalOpen(true)}>
                <TerminalSquare className="size-3.5 mr-1.5" />
                Terminal
              </Button>
            )}
            <Button variant="outline" size="sm" onClick={onNewSession}>
              <RotateCcw className="size-3.5 mr-1.5" />
              New Session
            </Button>
            <Button variant="destructive" size="sm" onClick={onKill}>Kill</Button>
          </div>
        </div>
      </header>

      <div ref={scrollRef} onScroll={handleScroll} className="flex-1 overflow-y-auto p-6 relative">
        <div className="space-y-4 max-w-3xl mx-auto">
          {messages.length === 0 ? (
            <p className="text-center text-muted-foreground py-12">No messages yet. Start the conversation below.</p>
          ) : (
            messages.map((message) => (
              <MessageBubble key={message.id} message={message} clawName={claw.name} />
            ))
          )}
          <div ref={bottomRef} className="h-[40vh]" />
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

      <div className="p-4 border-t border-border">
        <form onSubmit={handleSubmit} className="flex gap-2 items-end max-w-3xl mx-auto">
          <textarea
            value={input}
            onChange={(e) => {
              setInput(e.target.value)
              e.target.style.height = "auto"
              e.target.style.height = e.target.scrollHeight + "px"
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault()
                if (input.trim()) handleSubmit(e as unknown as React.FormEvent)
              }
            }}
            placeholder="Send a message to this claw..."
            rows={1}
            className="flex-1 resize-none overflow-hidden rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 min-h-[40px] max-h-[200px]"
          />
          <Button type="submit" size="icon" disabled={!input.trim()} className="shrink-0">
            <Send className="size-4" />
            <span className="sr-only">Send message</span>
          </Button>
        </form>
      </div>

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
  loading = false,
  hubError = null,
  messages,
  allMessages,
  onSendMessage,
  onSendMessageToClaw,
  onKill,
  onKillClaw,
  onNewSession,
  onNewSessionForClaw,
  onSelectClaw,
  onDeselectClaw,
}: ConversationViewProps) {
  const boardRef = useRef<HTMLDivElement>(null)

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
    // Stable order: sort by creation time (newest first) — never changes on state updates
    const sortedClaws = [...allClaws].sort((a, b) => {
      const ta = a.created_at ? new Date(a.created_at).getTime() : 0
      const tb = b.created_at ? new Date(b.created_at).getTime() : 0
      return tb - ta
    })

    return (
      <main className="flex-1 flex flex-col bg-background min-w-0 overflow-hidden">
        {/* Header */}
        <header className="flex items-center justify-between px-6 py-4 border-b border-border shrink-0">
          <div className="flex items-center gap-3">
            <Terminal className="size-5 text-muted-foreground" />
            <h2 className="text-lg font-medium text-foreground">
              {loading ? "Claws" : `${allClaws.length} Active Claws`}
            </h2>
          </div>
          <div className="flex items-center gap-4 text-xs text-muted-foreground">
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
        </header>

        {/* Board view */}
        <div className="flex-1 relative min-h-0">
          {sortedClaws.length === 0 && !loading ? (
            <div className="flex flex-col items-center justify-center h-full gap-6 px-8 text-center">
              <img src="/icon.svg" alt="ElasticClaw" className="size-20 opacity-20" />
              <div className="space-y-2">
                <p className="text-lg font-medium text-muted-foreground">No claws running</p>
                <p className="text-sm text-muted-foreground/70 max-w-sm">
                  Spawn your first claw from the CLI to get started.
                </p>
              </div>
              <div className="bg-muted rounded-lg px-4 py-3 font-mono text-sm text-foreground/80 max-w-md w-full text-left">
                <span className="text-muted-foreground select-none">$ </span>
                elasticclaw create --name my-claw
              </div>
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

          <div
            ref={boardRef}
            className="flex gap-4 h-full overflow-x-auto overflow-y-hidden py-6 px-12 items-stretch"
            style={{ scrollbarWidth: "none", msOverflowStyle: "none" }}
          >
            {sortedClaws.map((c) => (
              <ClawBoardCard
                key={c.id}
                claw={c}
                messages={(allMessages && allMessages[c.id]) || []}
                onClick={() => onSelectClaw(c.id)}
                onSendMessage={(content) => onSendMessageToClaw(c.id, content)}
                onNewSession={() => onNewSessionForClaw(c.id)}
                onKill={() => onKillClaw(c.id)}
              />
            ))}
          </div>

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
      claw={claw}
      messages={messages}
      onSendMessage={onSendMessage}
      onKill={onKill}
      onNewSession={onNewSession}
      onDeselectClaw={onDeselectClaw}
    />
  )
}
