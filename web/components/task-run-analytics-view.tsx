"use client"

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { AlertCircle, CheckCircle2, CircleDot, ExternalLink, GitPullRequest, RefreshCw, Search, XCircle } from "lucide-react"
import {
  fetchTaskRunAnalyticsSummary,
  fetchTaskRunAttempts,
  fetchTaskRunEvents,
  fetchTaskRunFilterOptions,
  fetchTaskRunPRs,
  fetchTaskRuns,
} from "@/lib/api"
import type {
  TaskRunAnalyticsFilters,
  TaskRunAnalyticsSummary,
  TaskRunAttempt,
  TaskRunEvent,
  TaskRunFilterOptions,
  TaskRunPR,
  TaskRunSummary,
} from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"

type DetailState = {
  attempts: TaskRunAttempt[]
  events: TaskRunEvent[]
  prs: TaskRunPR[]
}

type Cancellation = {
  cancelled: boolean
}

const anyValue = "__any__"
const DEFAULT_PAGE_LIMIT = 50
const MAX_DISPLAYED_EVENTS = 12

const urlFilterKeys = [
  "status",
  "factory",
  "workflow",
  "repo",
  "model",
  "warningType",
  "failureType",
] as const satisfies readonly (keyof TaskRunAnalyticsFilters)[]

const urlBooleanFilterKeys = [
  "humanTouched",
  "mergedPrs",
] as const satisfies readonly (keyof TaskRunAnalyticsFilters)[]

const allUrlFilterKeys = [
  ...urlFilterKeys,
  ...urlBooleanFilterKeys,
] as const satisfies readonly (keyof TaskRunAnalyticsFilters)[]

type UrlFilterKey = (typeof allUrlFilterKeys)[number]
type UrlStringFilterKey = (typeof urlFilterKeys)[number]
type KpiFilter = "runs" | "clean" | "warning" | "failed" | "humanTouches" | "mergedPrs"

function analyticsFiltersFromParams(params: URLSearchParams, workspaceScope?: string): TaskRunAnalyticsFilters {
  const filters: TaskRunAnalyticsFilters = {
    limit: DEFAULT_PAGE_LIMIT,
    analyticsEnabled: true,
    requiresPr: true,
  }
  if (workspaceScope) filters.workspace = workspaceScope
  for (const key of urlFilterKeys) {
    const value = params.get(key)
    if (value) filters[key] = value
  }
  for (const key of urlBooleanFilterKeys) {
    const value = booleanParam(params, key)
    if (value !== undefined) filters[key] = value
  }
  return filters
}

function booleanParam(params: URLSearchParams, key: string) {
  const value = params.get(key)
  if (value === "true") return true
  if (value === "false") return false
  return undefined
}

export function TaskRunAnalyticsView({ workspaceScope }: { workspaceScope?: string }) {
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const searchParamsKey = searchParams.toString()
  const filters = useMemo(
    () => analyticsFiltersFromParams(new URLSearchParams(searchParamsKey), workspaceScope),
    [searchParamsKey, workspaceScope]
  )
  const kpiFilters = useMemo<TaskRunAnalyticsFilters>(() => ({
    analyticsEnabled: true,
    requiresPr: true,
    ...(workspaceScope ? { workspace: workspaceScope } : {}),
  }), [workspaceScope])
  const [kpiSummary, setKpiSummary] = useState<TaskRunAnalyticsSummary | null>(null)
  const [runs, setRuns] = useState<TaskRunSummary[]>([])
  const [options, setOptions] = useState<TaskRunFilterOptions | null>(null)
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)
  const [details, setDetails] = useState<DetailState | null>(null)
  const [loading, setLoading] = useState(true)
  const [detailLoading, setDetailLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [detailError, setDetailError] = useState<string | null>(null)
  const optionsRef = useRef<TaskRunFilterOptions | null>(null)

  const selectedRun = useMemo(
    () => runs.find((run) => run.runId === selectedRunId) ?? null,
    [runs, selectedRunId]
  )

  const load = useCallback(async (cursor?: string, append = false, cancellation?: Cancellation) => {
    if (cancellation?.cancelled) return
    setLoading(true)
    setError(null)
    try {
      const query = { ...filters, cursor }
      if (append) {
        const runsData = await fetchTaskRuns(query)
        if (cancellation?.cancelled) return
        setRuns((prev) => [...prev, ...runsData.runs])
        setNextCursor(runsData.nextCursor)
        return
      }

      const [kpiSummaryData, runsData, optionsData] = await Promise.all([
        fetchTaskRunAnalyticsSummary(kpiFilters),
        fetchTaskRuns(query),
        optionsRef.current ? Promise.resolve(optionsRef.current) : fetchTaskRunFilterOptions(),
      ])
      if (cancellation?.cancelled) return
      setKpiSummary(kpiSummaryData)
      setRuns(runsData.runs)
      setNextCursor(runsData.nextCursor)
      if (!optionsRef.current) {
        optionsRef.current = optionsData
        setOptions(optionsData)
      }
      setSelectedRunId(null)
      setDetails(null)
      setDetailError(null)
    } catch (err) {
      if (cancellation?.cancelled) return
      setError(err instanceof Error ? err.message : "Unable to load analytics")
    } finally {
      if (cancellation?.cancelled) return
      setLoading(false)
    }
  }, [filters, kpiFilters])

  useEffect(() => {
    const cancellation: Cancellation = { cancelled: false }
    queueMicrotask(() => {
      if (cancellation.cancelled) return
      void load(undefined, false, cancellation)
    })
    return () => { cancellation.cancelled = true }
  }, [load])

  useEffect(() => {
    if (!selectedRunId) {
      return
    }
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setDetailLoading(true)
      setDetailError(null)
      setDetails(null)
      Promise.all([
        fetchTaskRunAttempts(selectedRunId),
        fetchTaskRunEvents(selectedRunId),
        fetchTaskRunPRs(selectedRunId),
      ])
        .then(([attempts, events, prs]) => {
          if (cancelled) return
          setDetails({ attempts: attempts.attempts, events: events.events, prs: prs.prs })
        })
        .catch((err) => {
          if (!cancelled) setDetailError(err instanceof Error ? err.message : "Unable to load run details")
        })
        .finally(() => {
          if (!cancelled) setDetailLoading(false)
        })
    })
    return () => { cancelled = true }
  }, [selectedRunId])

  const replaceFilterParams = useCallback((updates: Partial<Record<UrlFilterKey, string | boolean | undefined>>) => {
    const params = new URLSearchParams(searchParamsKey)
    for (const key of allUrlFilterKeys) {
      if (!(key in updates)) continue
      const value = updates[key]
      if (value !== undefined && value !== "") {
        params.set(key, String(value))
      } else {
        params.delete(key)
      }
    }
    params.delete("cursor")
    params.delete("workspace")
    params.delete("limit")
    params.delete("analyticsEnabled")
    params.delete("requiresPr")
    const query = params.toString()
    router.replace(query ? `${pathname}?${query}` : pathname, { scroll: false })
  }, [pathname, router, searchParamsKey])

  const setFilter = (key: UrlStringFilterKey, value: string | undefined) => {
    replaceFilterParams({ [key]: value || undefined })
  }

  const clearFilters = () => {
    const updates = Object.fromEntries(allUrlFilterKeys.map((key) => [key, undefined])) as Partial<Record<UrlFilterKey, undefined>>
    replaceFilterParams(updates)
  }

  const applyKpiFilter = (kpi: KpiFilter) => {
    const updates: Partial<Record<UrlFilterKey, string | boolean | undefined>> = {
      status: undefined,
      humanTouched: undefined,
      mergedPrs: undefined,
    }
    if (kpi === "clean") updates.status = "clean_success"
    if (kpi === "warning") updates.status = "warning_success"
    if (kpi === "failed") updates.status = "failed"
    if (kpi === "humanTouches") updates.humanTouched = true
    if (kpi === "mergedPrs") updates.mergedPrs = true
    replaceFilterParams(updates)
  }

  const activeKpi = useMemo<KpiFilter | null>(() => {
    if (filters.humanTouched === true) return "humanTouches"
    if (filters.mergedPrs === true) return "mergedPrs"
    if (filters.status === "clean_success") return "clean"
    if (filters.status === "warning_success") return "warning"
    if (filters.status === "failed") return "failed"
    if (!filters.status && filters.humanTouched === undefined && filters.mergedPrs === undefined) return "runs"
    return null
  }, [filters.humanTouched, filters.mergedPrs, filters.status])

  return (
    <main className="flex h-full min-w-0 bg-background">
      <section className="flex min-w-0 flex-1 flex-col">
        <header className="flex flex-col gap-3 border-b border-border px-4 py-3 lg:px-6">
          <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <div>
              <h2 className="text-lg font-semibold tracking-tight">Task Run Analytics</h2>
              <p className="text-sm text-muted-foreground">PR-scoped run outcomes, warnings, and delivery failures.</p>
            </div>
            <div className="flex w-fit items-center gap-2">
              <Button variant="ghost" size="sm" onClick={clearFilters}>Reset</Button>
              <Button variant="outline" size="sm" className="gap-2" onClick={() => load()}>
                <RefreshCw className={cn("size-4", loading && "animate-spin")} />
                Refresh
              </Button>
            </div>
          </div>
          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-6">
            <Metric label="Runs" value={kpiSummary?.totalRuns ?? 0} active={activeKpi === "runs"} onClick={() => applyKpiFilter("runs")} />
            <Metric label="Clean" value={kpiSummary?.byStatus.clean_success ?? 0} tone="success" active={activeKpi === "clean"} onClick={() => applyKpiFilter("clean")} />
            <Metric label="Warning" value={kpiSummary?.byStatus.warning_success ?? 0} tone="warning" active={activeKpi === "warning"} onClick={() => applyKpiFilter("warning")} />
            <Metric label="Failed" value={kpiSummary?.byStatus.failed ?? 0} tone="danger" active={activeKpi === "failed"} onClick={() => applyKpiFilter("failed")} />
            <Metric label="Human touches" value={kpiSummary?.humanInteractions ?? 0} active={activeKpi === "humanTouches"} onClick={() => applyKpiFilter("humanTouches")} />
            <Metric label="Merged PRs" value={kpiSummary?.prCounts.merged ?? 0} active={activeKpi === "mergedPrs"} onClick={() => applyKpiFilter("mergedPrs")} />
          </div>
          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-6">
            <FilterSelect label="Factory" value={filters.factory} values={options?.factories} onChange={(value) => setFilter("factory", value)} />
            <FilterSelect label="Workflow" value={filters.workflow} values={options?.workflows} onChange={(value) => setFilter("workflow", value)} />
            <FilterSelect label="Repo" value={filters.repo} values={options?.repos} onChange={(value) => setFilter("repo", value)} />
            <FilterSelect label="Model" value={filters.model} values={options?.models} onChange={(value) => setFilter("model", value)} />
            <FilterSelect label="Warning" value={filters.warningType} values={options?.warningTypes} onChange={(value) => setFilter("warningType", value)} />
            <FilterSelect label="Failure" value={filters.failureType} values={options?.failureTypes} onChange={(value) => setFilter("failureType", value)} />
          </div>
        </header>

        {error && (
          <div className="mx-4 mt-3 flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive lg:mx-6">
            <AlertCircle className="size-4" />
            {error}
          </div>
        )}

        <div className="min-h-0 flex-1 overflow-auto px-4 py-3 lg:px-6">
          <div className="overflow-hidden rounded-md border border-border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[140px]">Status</TableHead>
                  <TableHead>Owner</TableHead>
                  <TableHead>Repo</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Warnings</TableHead>
                  <TableHead className="text-right">PRs</TableHead>
                  <TableHead className="text-right">Started</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {runs.map((run) => (
                  <TableRow
                    key={run.runId}
                    data-state={run.runId === selectedRunId ? "selected" : undefined}
                    className="cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    tabIndex={0}
                    role="button"
                    onClick={() => setSelectedRunId(run.runId)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault()
                        setSelectedRunId(run.runId)
                      }
                    }}
                  >
                    <TableCell><StatusBadge status={run.status} /></TableCell>
                    <TableCell>
                      <div className="min-w-[180px]">
                        <div className="font-medium">{run.ownerDisplayName || run.factoryName || run.workflowName || run.ownerType}</div>
                        <div className="text-xs text-muted-foreground">{run.workspaceName || run.integration || "unknown"}</div>
                      </div>
                    </TableCell>
                    <TableCell>{run.repo || <span className="text-muted-foreground">None</span>}</TableCell>
                    <TableCell>{run.model || <span className="text-muted-foreground">Unknown</span>}</TableCell>
                    <TableCell>
                      <div className="flex max-w-[260px] flex-wrap gap-1">
                        {run.warningTypes.length === 0 ? (
                          <span className="text-xs text-muted-foreground">None</span>
                        ) : run.warningTypes.map((warning) => <Badge key={warning} variant="outline">{formatLabel(warning)}</Badge>)}
                      </div>
                    </TableCell>
                    <TableCell className="text-right">{run.mergedPrCount}/{run.prCount}</TableCell>
                    <TableCell className="text-right text-muted-foreground">{formatTime(run.startedAt)}</TableCell>
                  </TableRow>
                ))}
                {!loading && runs.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={7} className="h-32 text-center text-muted-foreground">
                      <Search className="mx-auto mb-2 size-5" />
                      No task runs match the current filters.
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
          {nextCursor && (
            <div className="mt-3 flex justify-center">
              <Button variant="outline" size="sm" onClick={() => load(nextCursor, true)} disabled={loading}>
                Load more
              </Button>
            </div>
          )}
        </div>
      </section>

      <RunDetailPanel run={selectedRun} details={details} loading={detailLoading} error={detailError} onClose={() => setSelectedRunId(null)} />
    </main>
  )
}

function Metric({ label, value, tone, active, onClick }: { label: string; value: number; tone?: "success" | "warning" | "danger"; active?: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "rounded-md border border-border bg-card px-3 py-2 text-left transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        active && "border-primary/60 bg-accent"
      )}
    >
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={cn(
        "mt-1 text-xl font-semibold",
        tone === "success" && "text-emerald-600 dark:text-emerald-400",
        tone === "warning" && "text-amber-600 dark:text-amber-400",
        tone === "danger" && "text-red-600 dark:text-red-400"
      )}>{value}</div>
    </button>
  )
}

function FilterSelect({ label, value, values, onChange }: { label: string; value?: string; values?: string[]; onChange: (value?: string) => void }) {
  const selectValues = value && !(values ?? []).includes(value)
    ? [value, ...(values ?? [])]
    : (values ?? [])
  const displayValue = value ? formatLabel(value) : "any"

  return (
    <Select value={value ?? anyValue} onValueChange={(next) => onChange(next === anyValue ? undefined : next)}>
      <SelectTrigger size="sm" className="w-full bg-background">
        <span className="truncate">
          <span className="text-muted-foreground">{label}:</span> {displayValue}
        </span>
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={anyValue}>{label}: any</SelectItem>
        {selectValues.map((item) => (
          <SelectItem key={item} value={item}>{formatLabel(item)}</SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function RunDetailPanel({ run, details, loading, error, onClose }: { run: TaskRunSummary | null; details: DetailState | null; loading: boolean; error: string | null; onClose: () => void }) {
  if (!run) return null
  return (
    <aside className="fixed inset-y-0 right-0 z-40 flex w-full max-w-[380px] shrink-0 flex-col border-l border-border bg-card shadow-xl xl:static xl:shadow-none">
      <div className="flex items-start justify-between gap-3 border-b border-border p-4">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold">{run.ownerDisplayName || run.runId}</div>
          <div className="truncate text-xs text-muted-foreground">{run.runId}</div>
        </div>
        <Button variant="ghost" size="icon" className="size-8" onClick={onClose} aria-label="Close detail panel">
          <XCircle className="size-4" />
        </Button>
      </div>
      <div className="min-h-0 flex-1 space-y-4 overflow-auto p-4">
        {error && (
          <div className="flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            <AlertCircle className="size-4" />
            {error}
          </div>
        )}
        <section className="grid grid-cols-2 gap-2 text-sm">
          <DetailItem label="Status" value={<StatusBadge status={run.status} />} />
          <DetailItem label="Phase" value={formatLabel(run.phase)} />
          <DetailItem label="Issue" value={run.issueId || "None"} />
          <DetailItem label="Started" value={formatTime(run.startedAt)} />
          <DetailItem label="Failure" value={run.failureType ? formatLabel(run.failureType) : "None"} />
          <DetailItem label="Human" value={String(run.humanInteractionCount)} />
        </section>
        <section>
          <h3 className="mb-2 text-xs font-medium uppercase text-muted-foreground">Pull Requests</h3>
          <div className="space-y-2">
            {(details?.prs ?? []).map((pr) => (
              <a key={pr.id} href={pr.url} target="_blank" rel="noreferrer" className="flex items-center justify-between rounded-md border border-border px-3 py-2 text-sm hover:bg-accent">
                <span className="flex min-w-0 items-center gap-2">
                  <GitPullRequest className="size-4 text-muted-foreground" />
                  <span className="truncate">{pr.repo}#{pr.prNumber}</span>
                </span>
                <ExternalLink className="size-3 text-muted-foreground" />
              </a>
            ))}
            {!loading && (details?.prs ?? []).length === 0 && <p className="text-sm text-muted-foreground">No PRs recorded.</p>}
          </div>
        </section>
        <section>
          <h3 className="mb-2 text-xs font-medium uppercase text-muted-foreground">Attempts</h3>
          <div className="space-y-2">
            {(details?.attempts ?? []).map((attempt) => (
              <div key={attempt.id} className="rounded-md border border-border px-3 py-2 text-sm">
                <div className="flex items-center justify-between">
                  <span>Attempt {attempt.attemptNumber}</span>
                  <Badge variant="outline">{attempt.status}</Badge>
                </div>
                {attempt.failureType && <div className="mt-1 text-xs text-muted-foreground">{formatLabel(attempt.failureType)}</div>}
              </div>
            ))}
          </div>
        </section>
        <section>
          <h3 className="mb-2 text-xs font-medium uppercase text-muted-foreground">Events</h3>
          <div className="space-y-2">
            {(details?.events ?? []).slice(-MAX_DISPLAYED_EVENTS).reverse().map((event) => (
              <div key={event.id} className="rounded-md border border-border px-3 py-2 text-sm">
                <div className="flex items-center justify-between gap-2">
                  <span className="truncate">{formatLabel(event.eventType)}</span>
                  <span className="text-xs text-muted-foreground">{formatTime(event.eventTime)}</span>
                </div>
                <div className="mt-1 text-xs text-muted-foreground">{event.actorLogin || event.actorType} · {event.source}</div>
              </div>
            ))}
          </div>
        </section>
      </div>
    </aside>
  )
}

function DetailItem({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="rounded-md border border-border px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-1 truncate">{value}</div>
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  const statusIcons: Record<string, ReactNode> = {
    clean_success: <CheckCircle2 className="size-3" />,
    failed: <XCircle className="size-3" />,
  }
  const icon = statusIcons[status] ?? <CircleDot className="size-3" />
  return (
    <Badge
      variant={status === "failed" ? "destructive" : status === "running" ? "secondary" : "outline"}
      className={cn(status === "clean_success" && "border-emerald-500/40 text-emerald-700 dark:text-emerald-300", status === "warning_success" && "border-amber-500/50 text-amber-700 dark:text-amber-300")}
    >
      {icon}
      {formatLabel(status)}
    </Badge>
  )
}

function formatLabel(value: string) {
  return value.replace(/_/g, " ").replace(/\b\w/g, (match) => match.toUpperCase())
}

function formatTime(value: number | null | undefined) {
  // The analytics API uses 0 as the absent timestamp sentinel for phase fields.
  if (value === null || value === undefined || value === 0) return "None"
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(new Date(value))
}
