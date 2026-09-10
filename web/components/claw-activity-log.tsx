"use client"

import { useEffect, useMemo, useState, type ReactNode } from "react"
import { AlertCircle, Loader2 } from "lucide-react"
import { ApiError, fetchActivityMessages } from "@/lib/api"
import type { AgentActivity, ApiMessage } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

const activityPageSize = 100

interface ActivityLogFetcher {
  fetchInitial: () => Promise<ApiMessage[]>
  fetchOlder: (before: string) => Promise<ApiMessage[]>
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
      fetchOlder: (before: string) => fetchActivityMessages(clawId, { before, limit: activityPageSize, order: "desc" }),
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
    const before = messages[0]?.created_at
    if (!before || loadingOlder) return
    setLoadingOlder(true)
    setError(null)
    try {
      const page = await activeFetcher.fetchOlder(before)
      setMessages((current) => [...page.reverse(), ...current])
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

function parseActivity(message: ApiMessage): AgentActivity | null {
  if (!message.format?.startsWith("activity:")) return null
  try {
    return JSON.parse(message.format.slice("activity:".length)) as AgentActivity
  } catch {
    return null
  }
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
