"use client"

import { useState, useEffect, useCallback, Suspense } from "react"
import { useSearchParams } from "next/navigation"
import { getHubUrl } from "@/lib/hub-url"
import { Button } from "@/components/ui/button"
import { ChevronLeft, RefreshCw, CheckCircle, XCircle, AlertCircle, Zap } from "lucide-react"
import { cn } from "@/lib/utils"

interface FactoryEvent {
  id: string
  factoryName: string
  issueId: string
  issueTitle: string
  prevStatus: string
  newStatus: string
  action: string
  clawId: string
  detail: string
  createdAt: string
}

function ActionBadge({ action }: { action: string }) {
  if (action === "claw_created") return (
    <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-green-500/15 text-green-400 font-medium">
      <CheckCircle className="size-3" /> claw created
    </span>
  )
  if (action === "claw_terminated") return (
    <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-blue-500/15 text-blue-400 font-medium">
      <CheckCircle className="size-3" /> claw terminated
    </span>
  )
  if (action === "error") return (
    <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-red-500/15 text-red-400 font-medium">
      <XCircle className="size-3" /> error
    </span>
  )
  return (
    <span className="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-muted text-muted-foreground font-medium">
      <AlertCircle className="size-3" /> not actionable
    </span>
  )
}

function formatTime(ts: string): string {
  const d = new Date(ts)
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) +
    " · " + d.toLocaleDateString([], { month: "short", day: "numeric" })
}

function FactoryEventsContent() {
  const searchParams = useSearchParams()
  const name = searchParams.get("name") ?? ""
  const [events, setEvents] = useState<FactoryEvent[]>([])
  const [loading, setLoading] = useState(true)
  const [lastRefresh, setLastRefresh] = useState<Date | null>(null)

  const load = useCallback(async () => {
    if (!name) return
    setLoading(true)
    try {
      const hubUrl = getHubUrl()
      const token = sessionStorage.getItem("ec_hub_token") || ""
      const res = await fetch(`${hubUrl}/api/factories/${encodeURIComponent(name)}/events`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (res.ok) setEvents(await res.json())
    } catch {}
    setLoading(false)
    setLastRefresh(new Date())
  }, [name])

  useEffect(() => { load() }, [load])

  // Auto-refresh every 30s
  useEffect(() => {
    if (!name) return
    const t = setInterval(load, 30_000)
    return () => clearInterval(t)
  }, [load, name])

  if (!name) {
    return (
      <div className="min-h-screen bg-background">
        <header className="border-b border-border px-6 py-4 flex items-center gap-4">
          <Button variant="ghost" size="icon" onClick={() => window.location.href = "/settings"}>
            <ChevronLeft className="size-4" />
          </Button>
        </header>
        <div className="max-w-3xl mx-auto px-6 py-8">
          <div className="text-center py-16 space-y-2">
            <p className="text-sm font-medium text-muted-foreground">Missing factory name</p>
            <p className="text-xs text-muted-foreground">Please provide a factory name in the URL query parameter.</p>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-background">
      <header className="border-b border-border px-6 py-4 flex items-center gap-4">
        <Button variant="ghost" size="icon" onClick={() => window.location.href = "/settings"}>
          <ChevronLeft className="size-4" />
        </Button>
        <Zap className="size-4 text-muted-foreground" />
        <div>
          <h1 className="text-base font-semibold">{name}</h1>
          <p className="text-xs text-muted-foreground">Factory activity · last 4 hours</p>
        </div>
        <div className="ml-auto flex items-center gap-3">
          {lastRefresh && (
            <span className="text-xs text-muted-foreground" suppressHydrationWarning>
              Updated {lastRefresh.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
            </span>
          )}
          <Button variant="outline" size="sm" onClick={load} disabled={loading}>
            <RefreshCw className={cn("size-3.5 mr-1.5", loading && "animate-spin")} />
            Refresh
          </Button>
        </div>
      </header>

      <div className="max-w-3xl mx-auto px-6 py-8">
        {loading && events.length === 0 ? (
          <p className="text-sm text-muted-foreground text-center py-12 animate-pulse">Loading…</p>
        ) : events.length === 0 ? (
          <div className="text-center py-16 space-y-2">
            <p className="text-sm font-medium text-muted-foreground">No events in the last 4 hours</p>
            <p className="text-xs text-muted-foreground">Webhooks will appear here as they arrive.</p>
          </div>
        ) : (
          <div className="space-y-2">
            {events.map((e) => (
              <div key={e.id} className={cn(
                "border border-border rounded-lg p-4 space-y-2",
                e.action === "not_actionable" && "opacity-60"
              )}>
                <div className="flex items-center justify-between gap-3">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="text-sm font-medium font-mono shrink-0">{e.issueId}</span>
                    <span className="text-sm text-muted-foreground truncate">{e.issueTitle}</span>
                  </div>
                  <ActionBadge action={e.action} />
                </div>
                <div className="flex items-center gap-3 text-xs text-muted-foreground">
                  {e.prevStatus && (
                    <>
                      <span className="bg-muted px-1.5 py-0.5 rounded">{e.prevStatus}</span>
                      <span>→</span>
                    </>
                  )}
                  <span className="bg-muted px-1.5 py-0.5 rounded">{e.newStatus}</span>
                  <span className="ml-auto" suppressHydrationWarning>{formatTime(e.createdAt)}</span>
                </div>
                {e.detail && (
                  <p className="text-xs text-muted-foreground">{e.detail}</p>
                )}
                {e.clawId && (
                  <p className="text-xs text-muted-foreground font-mono">claw: {e.clawId}</p>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}


export default function FactoryEventsPage() {
  return (
    <Suspense fallback={<div className="min-h-screen bg-background flex items-center justify-center"><p className="text-sm text-muted-foreground animate-pulse">Loading…</p></div>}>
      <FactoryEventsContent />
    </Suspense>
  )
}
