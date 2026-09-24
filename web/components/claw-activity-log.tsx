"use client"

import { useEffect, useMemo, useState, type ReactNode } from "react"
import { AlertCircle, ChevronDown, Loader2 } from "lucide-react"
import { ApiError, fetchActivityMessages } from "@/lib/api"
import type { AgentActivity, ApiMessage, WorkflowEffectEvent } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { cn } from "@/lib/utils"

const activityPageSize = 100

interface ActivityLogFetcher {
  fetchInitial: () => Promise<ApiMessage[]>
  // beforeId pairs with before as a compound (created_at, id) cursor so a page
  // boundary inside a same-timestamp group cannot skip its remaining rows.
  fetchOlder: (before: string, beforeId?: string) => Promise<ApiMessage[]>
}

export function ClawActivityLog({ clawId, fetcher }: { clawId?: string; fetcher?: ActivityLogFetcher }) {
  const [messages, setMessages] = useState<ApiMessage[]>([])
  const [loading, setLoading] = useState(false)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [hasOlder, setHasOlder] = useState(false)
  const [accessDenied, setAccessDenied] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const activeFetcher: ActivityLogFetcher | null = useMemo(() => {
    return fetcher || (clawId ? {
      fetchInitial: () => fetchActivityMessages(clawId, { limit: activityPageSize, order: "desc" }),
      fetchOlder: (before, beforeId) => fetchActivityMessages(clawId, { before, beforeId, limit: activityPageSize, order: "desc" }),
    } : null)
  }, [fetcher, clawId])

  useEffect(() => {
    if (!activeFetcher) return
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setLoading(true)
      setMessages([])
      setAccessDenied(false)
      setError(null)
      activeFetcher.fetchInitial()
        .then((page) => {
          if (cancelled) return
          setMessages(page.reverse())
          setHasOlder(page.length === activityPageSize)
        })
        .catch((err) => {
          if (cancelled) return
          if (err instanceof ApiError && err.status === 403) setAccessDenied(true)
          else setError(err instanceof Error ? err.message : "Unable to load agent activity")
        })
        .finally(() => { if (!cancelled) setLoading(false) })
    })
    return () => { cancelled = true }
  }, [activeFetcher])

  const loadOlder = async () => {
    if (!activeFetcher) return
    const oldest = messages[0]
    if (!oldest?.created_at || loadingOlder) return
    setLoadingOlder(true)
    setError(null)
    try {
      const page = await activeFetcher.fetchOlder(oldest.created_at, oldest.id)
      setMessages((current) => {
        const seen = new Set(current.map((message) => message.id))
        return [...page.reverse().filter((message) => !seen.has(message.id)), ...current]
      })
      setHasOlder(page.length === activityPageSize)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setAccessDenied(true)
      else setError(err instanceof Error ? err.message : "Unable to load older activity")
    } finally {
      setLoadingOlder(false)
    }
  }

  if (loading) return <LoadingState label="Loading run activity..." />
  if (accessDenied) return <Notice>You don&apos;t have access to this run&apos;s logs.</Notice>
  if (error && messages.length === 0) return <Notice destructive>{error}</Notice>
  if (messages.length === 0) return <EmptyState>No activity or state transitions were recorded for this attempt.</EmptyState>

  return (
    <div className="flex h-full min-h-0 flex-col rounded-md border">
      <div className="shrink-0 border-b p-2 text-center">
        {hasOlder ? (
          <Button variant="ghost" size="sm" onClick={loadOlder} disabled={loadingOlder}>
            {loadingOlder && <Loader2 className="size-4 animate-spin" />}
            Load older
          </Button>
        ) : <span className="text-xs text-muted-foreground">Beginning of activity</span>}
      </div>
      {error && <div className="border-b px-3 py-2 text-sm text-destructive">{error}</div>}
      <div className="min-h-0 flex-1 divide-y overflow-auto">
        {messages.map((message) => <ActivityLine key={message.id} message={message} />)}
      </div>
    </div>
  )
}

function ActivityLine({ message }: { message: ApiMessage }) {
  const effectEvent = parseWorkflowEffectEvent(message)
  if (effectEvent) return <WorkflowEffectLine message={message} event={effectEvent} />
  const activity = parseActivity(message)
  const isState = message.role === "state"
  const label = activity?.tool || activity?.kind || (isState ? "state" : "activity")
  const detailItems = activity ? [
    activity.command && { kind: "command", value: activity.command },
    activity.path && { kind: "path", value: activity.path },
    activity.url && { kind: "url", value: activity.url },
    activity.detail && { kind: "detail", value: activity.detail },
    activity.error && { kind: "error", value: activity.error },
    !activity.detail && !activity.error && activity.message && { kind: "message", value: activity.message },
  ].filter((item): item is { kind: string; value: string } => Boolean(item)) : []

  return (
    <div className="grid gap-2 px-3 py-3 text-sm sm:grid-cols-[8rem_10rem_minmax(0,1fr)]">
      <time className="text-xs text-muted-foreground">{formatTimestamp(message.created_at)}</time>
      <div className="flex min-w-0 items-start gap-2">
        <span className={cn("truncate font-medium", isState && "text-primary")}>{label}</span>
        {isState && <Badge variant="outline" className="shrink-0 text-[10px]">state</Badge>}
        {activity?.phase && <Badge variant="outline" className="shrink-0 text-[10px]">{activity.phase}</Badge>}
      </div>
      <div className="min-w-0 space-y-1">
        {detailItems.length > 0 ? detailItems.map((item, index) => (
          item.kind === "url" && isSafeHttpUrl(item.value) ? (
            <a key={`${item.kind}-${index}`} href={item.value} target="_blank" rel="noreferrer" className="block truncate text-primary underline-offset-4 hover:underline">{item.value}</a>
          ) : (
            <div key={`${item.kind}-${index}`} className={cn("break-words text-muted-foreground", item.kind === "command" && "rounded bg-muted px-2 py-1 font-mono text-foreground", item.kind === "error" && "text-destructive")}>{item.value}</div>
          )
        )) : <span className="text-muted-foreground">{message.content || "No details"}</span>}
      </div>
    </div>
  )
}

function isSafeHttpUrl(value: string) {
  try {
    const protocol = new URL(value).protocol
    return protocol === "http:" || protocol === "https:"
  } catch {
    return false
  }
}

// WorkflowEffectLine renders a workflow v2 effect or agent-task lifecycle log
// line: status, the command that ran, and collapsible stdout/stderr (or the
// instructions given to the agent) so run logs explain what the workflow did.
function WorkflowEffectLine({ message, event }: { message: ApiMessage; event: WorkflowEffectEvent }) {
  const failed = event.succeeded === false ||
    ["permanent_failed", "retryable_failed", "failed", "timed_out", "unknown"].includes(event.status ?? "")
  return (
    <div className="grid gap-2 px-3 py-3 text-sm sm:grid-cols-[8rem_10rem_minmax(0,1fr)]">
      <time className="text-xs text-muted-foreground">{formatTimestamp(message.created_at)}</time>
      <div className="flex min-w-0 items-start gap-2">
        <span className="truncate font-medium text-primary">{event.kind}</span>
        <Badge variant="outline" className="shrink-0 text-[10px]">effect</Badge>
      </div>
      <div className="min-w-0 space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant={failed ? "destructive" : "secondary"}>{event.status || event.phase}</Badge>
          {event.attempt ? <span className="text-xs text-muted-foreground">attempt {event.attempt}</span> : null}
          <span className="text-xs text-muted-foreground">{message.content}</span>
        </div>
        {event.command && <div className="break-all rounded bg-muted px-2 py-1 font-mono text-xs text-foreground">{event.command}</div>}
        {event.exit_code !== undefined && <div className="text-xs text-muted-foreground">exit code {event.exit_code}</div>}
        {event.error && <div className="break-words text-destructive">{event.error}</div>}
        {event.terminal_reason && <div className="break-words text-destructive">{event.terminal_reason}</div>}
        <OutputBlock label="stdout" value={event.stdout} />
        <OutputBlock label="stderr" value={event.stderr} />
        <OutputBlock label="instructions" value={event.instructions} />
      </div>
    </div>
  )
}

function OutputBlock({ label, value }: { label: string; value?: string }) {
  if (!value) return null
  const lineCount = value.trim().length === 0 ? 0 : value.trim().split("\n").length
  return (
    <Collapsible>
      <CollapsibleTrigger asChild>
        <Button variant="ghost" size="sm" className="h-7 gap-1 px-2 text-xs text-muted-foreground">
          <ChevronDown className="size-3" />
          {label} ({lineCount} line{lineCount === 1 ? "" : "s"})
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-2 font-mono text-xs">{value}</pre>
      </CollapsibleContent>
    </Collapsible>
  )
}

function parseActivity(message: ApiMessage): AgentActivity | null {
  if (!message.format?.startsWith("activity:")) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(message.format.slice("activity:".length))
  } catch {
    return null
  }
  if (!isRecord(parsed) || typeof parsed.kind !== "string") return null
  return {
    kind: parsed.kind,
    stream: optionalString(parsed, "stream"),
    phase: optionalString(parsed, "phase"),
    tool: optionalString(parsed, "tool"),
    detail: optionalString(parsed, "detail"),
    command: optionalString(parsed, "command"),
    path: optionalString(parsed, "path"),
    url: optionalString(parsed, "url"),
    message: optionalString(parsed, "message"),
    error: optionalString(parsed, "error"),
    call_id: optionalString(parsed, "call_id"),
    duration_ms: optionalNumber(parsed, "duration_ms"),
    exit_code: optionalNumber(parsed, "exit_code"),
    result: optionalString(parsed, "result"),
    subagent_name: optionalString(parsed, "subagent_name"),
    subagent_type: optionalString(parsed, "subagent_type"),
    subagent_model: optionalString(parsed, "subagent_model"),
    subagent_prompt: optionalString(parsed, "subagent_prompt"),
  }
}

const WORKFLOW_EFFECT_PHASES: ReadonlySet<string> = new Set(["planned", "started", "finished", "assigned"])

// parseWorkflowEffectEvent decodes the hub-authored "workflow:effect:<json>"
// format. The payload is validated field by field so a malformed row degrades
// to a plain-text line instead of crashing the log view.
function parseWorkflowEffectEvent(message: ApiMessage): WorkflowEffectEvent | null {
  if (!message.format?.startsWith("workflow:effect:")) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(message.format.slice("workflow:effect:".length))
  } catch {
    return null
  }
  if (!isRecord(parsed) || typeof parsed.kind !== "string" ||
    typeof parsed.phase !== "string" || !WORKFLOW_EFFECT_PHASES.has(parsed.phase)) {
    return null
  }
  return {
    kind: parsed.kind,
    phase: parsed.phase as WorkflowEffectEvent["phase"],
    status: optionalString(parsed, "status"),
    attempt: optionalNumber(parsed, "attempt"),
    definition_path: optionalString(parsed, "definition_path"),
    command: optionalString(parsed, "command"),
    exit_code: optionalNumber(parsed, "exit_code"),
    succeeded: typeof parsed.succeeded === "boolean" ? parsed.succeeded : undefined,
    stdout: optionalString(parsed, "stdout"),
    stderr: optionalString(parsed, "stderr"),
    error: optionalString(parsed, "error"),
    instructions: optionalString(parsed, "instructions"),
    terminal_reason: optionalString(parsed, "terminal_reason"),
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null
}

function optionalString(record: Record<string, unknown>, key: string): string | undefined {
  return typeof record[key] === "string" ? (record[key] as string) : undefined
}

function optionalNumber(record: Record<string, unknown>, key: string): number | undefined {
  return typeof record[key] === "number" ? (record[key] as number) : undefined
}

export function LoadingState({ label }: { label: string }) {
  return <div className="flex h-40 items-center justify-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{label}</div>
}

export function EmptyState({ children }: { children: ReactNode }) {
  return <div className="flex h-40 items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">{children}</div>
}

export function Notice({ children, destructive = false }: { children: ReactNode; destructive?: boolean }) {
  return <div className={cn("flex items-center gap-2 rounded-md border px-4 py-3 text-sm", destructive && "border-destructive/30 bg-destructive/10 text-destructive")}><AlertCircle className={cn("size-4 shrink-0", destructive && "text-destructive")} />{children}</div>
}

function formatTimestamp(value: string) {
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(value))
}
