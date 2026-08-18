"use client"

import { useEffect, useMemo, useState } from "react"
import { FileTerminal } from "lucide-react"
import { fetchTaskRunOutputs } from "@/lib/api"
import type { TaskRunAttempt, TaskRunOutput, TaskRunSummary } from "@/lib/types"
import { useEscapeToClose } from "@/hooks/use-escape-to-close"
import { AttrChip, SEVERITY, SeverityChip } from "@/components/ds"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { ClawActivityLog, EmptyState, LoadingState, Notice } from "@/components/claw-activity-log"
import { cn } from "@/lib/utils"

export function RunLogsDialog({ run, attempts }: { run: TaskRunSummary; attempts: TaskRunAttempt[] }) {
  const [open, setOpen] = useState(false)
  const [tab, setTab] = useState("actions")
  const [outputs, setOutputs] = useState<TaskRunOutput[]>([])
  const [outputsLoading, setOutputsLoading] = useState(false)
  const [outputsError, setOutputsError] = useState<string | null>(null)
  const [traceId, setTraceId] = useState<string>()
  const attemptOptions = useMemo(() => attempts.filter((attempt) => attempt.clawId), [attempts])
  const defaultClawId = useMemo(
    () => attemptOptions.find((attempt) => attempt.attemptId === run.currentAttemptId)?.clawId || run.clawId,
    [attemptOptions, run.clawId, run.currentAttemptId]
  )
  const [selectedClawId, setSelectedClawId] = useState<string | null>(null)
  const clawId = selectedClawId ?? defaultClawId

  const disabled = !run.clawId
  // A run that died before the agent started (e.g. failed provisioning) has
  // claw-linked attempts but no agent activity to show.
  const hasAgentActivity = attemptOptions.length > 0 && Boolean(run.agentStartedAt)
  useEscapeToClose(() => setOpen(false), open)
  useEffect(() => {
    if (!open) return
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setOutputsLoading(true)
      setOutputsError(null)
      fetchTaskRunOutputs(run.runId)
        .then((response) => {
          if (cancelled) return
          setOutputs(response.outputs)
          setTraceId(response.traceId)
          // A run that never reached the agent has no activity—open its pipeline output.
          if (!hasAgentActivity && response.outputs.length > 0) setTab("output")
        })
        .catch((err) => { if (!cancelled) setOutputsError(err instanceof Error ? err.message : "Unable to load pipeline output") })
        .finally(() => { if (!cancelled) setOutputsLoading(false) })
    })
    return () => { cancelled = true }
  }, [hasAgentActivity, open, run.runId])
  return (
    <>
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex" tabIndex={disabled ? 0 : undefined}>
            <Button variant="outline" size="sm" disabled={disabled} onClick={() => { setTab(hasAgentActivity ? "actions" : "output"); setOpen(true) }}>
              <FileTerminal className="size-4" />
              Agent logs
            </Button>
          </span>
        </TooltipTrigger>
        {disabled && <TooltipContent>This run is not linked to an agent, so logs are unavailable.</TooltipContent>}
      </Tooltip>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent overlayClassName="z-[70]" className="z-[71] flex h-[min(85vh,800px)] flex-col gap-3 overflow-hidden p-0 sm:max-w-5xl">
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
              <ClawActivityLog clawId={clawId} />
            </TabsContent>
            <TabsContent value="output" className="min-h-0 overflow-auto">
              <OutputTab outputs={outputs} loading={outputsLoading} error={outputsError} traceId={traceId} workspaceName={run.workspaceName} />
            </TabsContent>
          </Tabs>
        </DialogContent>
      </Dialog>
    </>
  )
}

function OutputTab({ outputs, loading, error, traceId, workspaceName }: { outputs: TaskRunOutput[]; loading: boolean; error: string | null; traceId?: string; workspaceName: string }) {
  const [minSeverity, setMinSeverity] = useState(0)
  const [raw, setRaw] = useState(false)
  const groups = useMemo(() => {
    const grouped = new Map<string, TaskRunOutput[]>()
    for (const output of outputs) {
      const stage = output.stageId || "Unspecified stage"
      const stageOutputs = grouped.get(stage)
      if (stageOutputs) stageOutputs.push(output)
      else grouped.set(stage, [output])
    }
    return [...grouped.entries()]
  }, [outputs])

  if (loading) return <LoadingState label="Loading pipeline output..." />
  if (error) return <Notice destructive>{error}</Notice>
  if (outputs.length === 0) return <EmptyState>No pipeline output was recorded for this run.</EmptyState>
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-card px-3 py-2 font-mono text-[11px]">
        <AttrChip k="service.name" v="elasticclaw-pipeline" /><AttrChip k="deployment.environment" v={workspaceName} /><AttrChip k="trace_id" v={traceId || "—"} />
        <div className="ml-auto flex items-center gap-1">{([ ["ALL", 0], ["DEBUG", SEVERITY.DEBUG.rank], ["INFO", SEVERITY.INFO.rank], ["WARN", SEVERITY.WARN.rank], ["ERROR", SEVERITY.ERROR.rank] ] as const).map(([name, rank]) => <Button key={name} variant={minSeverity === rank ? "secondary" : "ghost"} size="sm" className="h-6 px-2 font-mono text-[10px]" onClick={() => setMinSeverity(rank)}>{name}</Button>)}<Button variant={raw ? "secondary" : "ghost"} size="sm" className="h-6 px-2 font-mono text-[10px]" onClick={() => setRaw((value) => !value)}>RAW</Button></div>
      </div>
      {groups.map(([stage, stageOutputs]) => (
        <section key={stage} className="rounded-md border">
          <h3 className="border-b bg-muted/40 px-4 py-2 text-sm font-semibold">{stage}</h3>
          <div className="divide-y">
            {stageOutputs.map((output) => <OutputBlock key={`${output.clawId}-${output.outputName}`} output={output} minSeverity={minSeverity} raw={raw} />)}
          </div>
        </section>
      ))}
    </div>
  )
}

function OutputBlock({ output, minSeverity, raw }: { output: TaskRunOutput; minSeverity: number; raw: boolean }) {
  const { records, counts } = useMemo(() => {
    const counts: Record<string, number> = {}
    const records = output.records.filter((record) => {
      counts[record.sev] = (counts[record.sev] || 0) + 1
      return record.severityNumber >= minSeverity
    })
    return { records, counts }
  }, [output.records, minSeverity])
  const showRaw = raw || output.records.length === 0
  return (
    <div className="space-y-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="text-sm font-medium">{output.outputName}</div>
          <div className="font-mono text-xs text-muted-foreground">{output.spanKind} · span_id={output.spanId} · {(output.durationMs / 1000).toFixed(2)}s · exit={output.exitCode}</div>
        </div>
        <Badge className="border" style={{ borderColor: `color-mix(in srgb, var(--chart-${output.status === "OK" ? "2" : "4"}) 30%, transparent)`, backgroundColor: `color-mix(in srgb, var(--chart-${output.status === "OK" ? "2" : "4"}) 15%, transparent)`, color: `var(--chart-${output.status === "OK" ? "2" : "4"})` }}>{output.status}</Badge>
      </div>
      {showRaw ? <>{output.stdout && <LogStream label="stdout" value={output.stdout} />}{output.stderr && <LogStream label="stderr" value={output.stderr} error />}{!output.stdout && !output.stderr && <p className="text-sm text-muted-foreground">This output did not write to stdout or stderr.</p>}</> : records.length ? <div className="divide-y rounded-md border">{records.map((record, index) => <div key={index} className="grid grid-cols-[auto_auto_1fr] gap-2 px-3 py-2 font-mono text-xs"><span className="text-muted-foreground">{new Date(record.ts).toLocaleTimeString()}</span><SeverityChip severity={record.sev} /><span className={record.severityNumber >= SEVERITY.ERROR.rank ? "text-destructive" : ""}>{record.body} {Object.entries(record.attrs || {}).map(([key, value]) => <AttrChip key={key} k={key} v={value as string | number | boolean | null | undefined} />)}</span></div>)}</div> : <p className="text-sm text-muted-foreground">No records at this severity.</p>}
      <div className="flex gap-2 border-t pt-2 font-mono text-[11px] text-muted-foreground"><span>{output.records.length} records</span>{(["WARN", "ERROR", "FATAL"] as const).map((severity) => counts[severity] ? <span key={severity} style={{ color: severity === "FATAL" ? "var(--text-error)" : SEVERITY[severity].color }}>{counts[severity]} {severity.toLowerCase()}</span> : null)}<span className="ml-auto">{output.attemptId}</span></div>
    </div>
  )
}

function LogStream({ label, value, error = false }: { label: string; value: string; error?: boolean }) {
  return <div><div className={cn("mb-1 text-xs font-medium text-muted-foreground", error && "text-destructive")}>{label}</div><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted p-3 font-mono text-xs">{value}</pre></div>
}
