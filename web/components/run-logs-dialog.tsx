"use client"

import { useEffect, useMemo, useState, type ReactNode } from "react"
import { AlertCircle, FileTerminal, Loader2 } from "lucide-react"
import { ApiError, fetchActivityMessages, fetchTaskRunOutputs } from "@/lib/api"
import type { AgentActivity, ApiMessage, TaskRunAttempt, TaskRunOutput, TaskRunSummary } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"

const activityPageSize = 100

export function RunLogsDialog({ run, attempts }: { run: TaskRunSummary; attempts: TaskRunAttempt[] }) {
  const [open, setOpen] = useState(false)
  const [tab, setTab] = useState("actions")
  const attemptOptions = useMemo(() => attempts.filter((attempt) => attempt.clawId), [attempts])
  const defaultClawId = useMemo(
    () => attemptOptions.find((attempt) => attempt.attemptId === run.currentAttemptId)?.clawId || run.clawId,
    [attemptOptions, run.clawId, run.currentAttemptId]
  )
  const [selectedClawId, setSelectedClawId] = useState<string | null>(null)
  const clawId = selectedClawId ?? defaultClawId

  const disabled = !run.clawId
  return (
    <>
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex" tabIndex={disabled ? 0 : undefined}>
            <Button variant="outline" size="sm" disabled={disabled} onClick={() => setOpen(true)}>
              <FileTerminal className="size-4" />
              Agent logs
            </Button>
          </span>
        </TooltipTrigger>
        {disabled && <TooltipContent>This run is not linked to an agent, so logs are unavailable.</TooltipContent>}
      </Tooltip>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="flex h-[min(85vh,800px)] flex-col gap-3 overflow-hidden p-0 sm:max-w-5xl">
          <DialogHeader className="shrink-0 border-b px-6 py-5 pr-12">
            <DialogTitle>Agent logs</DialogTitle>
            <DialogDescription>Agent activity and pipeline output for {run.ownerDisplayName || run.runId}.</DialogDescription>
          </DialogHeader>
          <Tabs value={tab} onValueChange={setTab} className="min-h-0 flex-1 gap-0 px-6 pb-6">
            <div className="flex shrink-0 items-center justify-between gap-3 pb-3">
              <TabsList>
                <TabsTrigger value="actions">Actions</TabsTrigger>
                <TabsTrigger value="output">Output</TabsTrigger>
              </TabsList>
              {tab === "actions" && attemptOptions.length > 1 && (
                <Select value={clawId} onValueChange={setSelectedClawId}>
                  <SelectTrigger size="sm" className="w-44">
                    <SelectValue placeholder="Select attempt" />
                  </SelectTrigger>
                  <SelectContent>
                    {attemptOptions.map((attempt) => (
                      <SelectItem key={attempt.id} value={attempt.clawId}>Attempt {attempt.attemptNumber}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            </div>
            <TabsContent value="actions" className="min-h-0 overflow-hidden">
              <ActionsTab open={open} clawId={clawId} />
            </TabsContent>
            <TabsContent value="output" className="min-h-0 overflow-auto">
              <OutputTab open={open} runId={run.runId} />
            </TabsContent>
          </Tabs>
        </DialogContent>
      </Dialog>
    </>
  )
}

function ActionsTab({ open, clawId }: { open: boolean; clawId: string }) {
  const [messages, setMessages] = useState<ApiMessage[]>([])
  const [loading, setLoading] = useState(false)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [hasOlder, setHasOlder] = useState(false)
  const [accessDenied, setAccessDenied] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open || !clawId) return
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setLoading(true)
      setMessages([])
      setAccessDenied(false)
      setError(null)
      fetchActivityMessages(clawId, { limit: activityPageSize, order: "desc" })
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
  }, [clawId, open])

  const loadOlder = async () => {
    const before = messages[0]?.created_at
    if (!before || loadingOlder) return
    setLoadingOlder(true)
    setError(null)
    try {
      const page = await fetchActivityMessages(clawId, { before, limit: activityPageSize, order: "desc" })
      setMessages((current) => [...page.reverse(), ...current])
      setHasOlder(page.length === activityPageSize)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setAccessDenied(true)
      else setError(err instanceof Error ? err.message : "Unable to load older activity")
    } finally {
      setLoadingOlder(false)
    }
  }

  if (loading) return <LoadingState label="Loading agent activity..." />
  if (accessDenied) return <Notice>You don&apos;t have access to this agent&apos;s logs.</Notice>
  if (error && messages.length === 0) return <Notice destructive>{error}</Notice>
  if (messages.length === 0) return <EmptyState>No agent actions were recorded for this attempt.</EmptyState>

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
  const label = activity?.tool || activity?.kind || "activity"
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
        <span className="truncate font-medium">{label}</span>
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

function OutputTab({ open, runId }: { open: boolean; runId: string }) {
  const [outputs, setOutputs] = useState<TaskRunOutput[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!open) return
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setLoading(true)
      setError(null)
      fetchTaskRunOutputs(runId)
        .then((response) => { if (!cancelled) setOutputs(response.outputs) })
        .catch((err) => { if (!cancelled) setError(err instanceof Error ? err.message : "Unable to load pipeline output") })
        .finally(() => { if (!cancelled) setLoading(false) })
    })
    return () => { cancelled = true }
  }, [open, runId])

  const groups = useMemo(() => {
    const grouped = new Map<string, TaskRunOutput[]>()
    for (const output of outputs) grouped.set(output.stageId || "Unspecified stage", [...(grouped.get(output.stageId || "Unspecified stage") || []), output])
    return [...grouped.entries()]
  }, [outputs])

  if (loading) return <LoadingState label="Loading pipeline output..." />
  if (error) return <Notice destructive>{error}</Notice>
  if (outputs.length === 0) return <EmptyState>No pipeline output was recorded for this run.</EmptyState>
  return (
    <div className="space-y-4">
      {groups.map(([stage, stageOutputs]) => (
        <section key={stage} className="rounded-md border">
          <h3 className="border-b bg-muted/40 px-4 py-2 text-sm font-semibold">{stage}</h3>
          <div className="divide-y">
            {stageOutputs.map((output) => <OutputBlock key={`${output.clawId}-${output.outputName}`} output={output} />)}
          </div>
        </section>
      ))}
    </div>
  )
}

function OutputBlock({ output }: { output: TaskRunOutput }) {
  return (
    <div className="space-y-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="text-sm font-medium">{output.outputName}</div>
          {output.attemptId && <div className="text-xs text-muted-foreground">{output.attemptId}</div>}
        </div>
        <Badge className={cn("border", output.exitCode === 0 ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" : "border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-300")}>Exit {output.exitCode}</Badge>
      </div>
      {output.stdout && <LogStream label="stdout" value={output.stdout} />}
      {output.stderr && <LogStream label="stderr" value={output.stderr} error />}
      {!output.stdout && !output.stderr && <p className="text-sm text-muted-foreground">This output did not write to stdout or stderr.</p>}
    </div>
  )
}

function LogStream({ label, value, error = false }: { label: string; value: string; error?: boolean }) {
  return <div><div className={cn("mb-1 text-xs font-medium text-muted-foreground", error && "text-destructive")}>{label}</div><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted p-3 font-mono text-xs">{value}</pre></div>
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

function LoadingState({ label }: { label: string }) {
  return <div className="flex h-40 items-center justify-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{label}</div>
}

function EmptyState({ children }: { children: ReactNode }) {
  return <div className="flex h-40 items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">{children}</div>
}

function Notice({ children, destructive = false }: { children: ReactNode; destructive?: boolean }) {
  return <div className={cn("flex items-center gap-2 rounded-md border px-4 py-3 text-sm", destructive && "border-destructive/30 bg-destructive/10 text-destructive")}><AlertCircle className="size-4 shrink-0" />{children}</div>
}

function formatTimestamp(value: string) {
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(value))
}
