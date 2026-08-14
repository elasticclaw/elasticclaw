"use client"

import { useEffect, useState, type ReactNode } from "react"
import { useRouter } from "next/navigation"
import { AlertCircle, BarChart3, Calendar, ExternalLink, History, Loader2 } from "lucide-react"
import { ApiError, fetchCronWorkflowRuns, type Workflow } from "@/lib/api"
import { WorkflowRunLogsDialog } from "@/components/workflow-run-logs-dialog"
import type { WorkflowRun } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"

export function WorkflowRunsDialog({ workflow }: { workflow: Workflow }) {
  const router = useRouter()
  const [open, setOpen] = useState(false)
  const [runs, setRuns] = useState<WorkflowRun[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) return
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setLoading(true)
      setError(null)
      setRuns([])
      fetchCronWorkflowRuns(workflow.workspaceName, workflow.name)
        .then((response) => {
          if (cancelled) return
          setRuns(response.runs || [])
        })
        .catch((err) => {
          if (cancelled) return
          if (err instanceof ApiError && err.status === 404) {
            setError("This workflow is not configured for scheduled or manual runs.")
            return
          }
          setError(err instanceof Error ? err.message : "Unable to load run history")
        })
        .finally(() => {
          if (!cancelled) setLoading(false)
        })
    })
    return () => { cancelled = true }
  }, [open, workflow.workspaceName, workflow.name])

  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        <History className="size-4" />
        Run history
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="flex h-[min(85vh,700px)] flex-col gap-3 overflow-hidden p-0 sm:max-w-4xl">
          <DialogHeader className="shrink-0 border-b px-6 py-5 pr-12">
            <DialogTitle>Run history</DialogTitle>
            <DialogDescription>
              Recent executions of <span className="font-medium">{workflow.name}</span> in{" "}
              <span className="font-medium">{workflow.workspaceName}</span>.
            </DialogDescription>
          </DialogHeader>
          <div className="min-h-0 flex-1 overflow-y-auto px-6 pb-6">
            {loading && <LoadingState label="Loading run history…" />}
            {!loading && error && <Notice destructive>{error}</Notice>}
            {!loading && !error && runs.length === 0 && (
              <EmptyState>No runs have been recorded for this workflow yet.</EmptyState>
            )}
            {!loading && !error && runs.length > 0 && (
              <div className="rounded-md border">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-28">Status</TableHead>
                      <TableHead className="w-24">Trigger</TableHead>
                      <TableHead>Started</TableHead>
                      <TableHead>Finished</TableHead>
                      <TableHead>Result</TableHead>
                      <TableHead>Agent</TableHead>
                      <TableHead className="w-28">Logs</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {runs.map((run) => (
                      <TableRow key={run.id}>
                        <TableCell>
                          <StatusBadge status={run.status} />
                        </TableCell>
                        <TableCell className="text-xs capitalize">{run.trigger_type}</TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {run.started_at ? formatTimestamp(run.started_at) : "—"}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {run.finished_at ? formatTimestamp(run.finished_at) : "—"}
                        </TableCell>
                        <TableCell className="min-w-[20rem] max-w-lg align-top">
                          <div className="max-w-full text-xs text-muted-foreground whitespace-pre-wrap break-words leading-relaxed">
                            {run.result || "—"}
                          </div>
                        </TableCell>
                        <TableCell className="text-xs font-mono text-muted-foreground">
                          {run.claw_id ? shortId(run.claw_id) : "—"}
                        </TableCell>
                        <TableCell>
                          <WorkflowRunLogsDialog run={run} />
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </div>
          <div className="shrink-0 border-t bg-background px-6 py-3">
            <Button
              variant="ghost"
              size="sm"
              className="h-auto px-0 py-0 text-xs text-primary hover:text-primary hover:underline"
              onClick={() => {
                setOpen(false)
                router.push(
                  `/analytics?workspace=${encodeURIComponent(workflow.workspaceName)}&workflow=${encodeURIComponent(workflow.name)}`
                )
              }}
            >
              <BarChart3 className="size-3.5" />
              Open full analytics for this workflow
              <ExternalLink className="size-3" />
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

function StatusBadge({ status }: { status: string }) {
  const variant = statusVariant(status)
  return (
    <Badge className={cn("text-[10px]", variant)}>
      {status}
    </Badge>
  )
}

function statusVariant(status: string) {
  switch (status.toLowerCase()) {
    case "completed":
    case "success":
      return "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
    case "failed":
    case "error":
      return "border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-300"
    case "running":
      return "border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-300"
    case "skipped":
      return "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300"
    case "pending":
    default:
      return "border-border bg-muted text-muted-foreground"
  }
}

function LoadingState({ label }: { label: string }) {
  return (
    <div className="flex h-40 items-center justify-center gap-2 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      {label}
    </div>
  )
}

function EmptyState({ children }: { children: ReactNode }) {
  return (
    <div className="flex h-40 items-center justify-center gap-2 rounded-md border border-dashed text-sm text-muted-foreground">
      <Calendar className="size-4" />
      {children}
    </div>
  )
}

function Notice({ children, destructive = false }: { children: ReactNode; destructive?: boolean }) {
  return (
    <div className={cn(
      "flex items-center gap-2 rounded-md border px-4 py-3 text-sm",
      destructive && "border-destructive/30 bg-destructive/10 text-destructive"
    )}>
      <AlertCircle className={cn("size-4 shrink-0", destructive && "text-destructive")} />
      {children}
    </div>
  )
}

function shortId(id: string) {
  return id.length > 8 ? id.slice(0, 8) : id
}

function formatTimestamp(value: string) {
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(new Date(value))
}
