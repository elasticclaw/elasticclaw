"use client"

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from "react"
import { createPortal } from "react-dom"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { AlertCircle, ArrowDown, ArrowUp, ExternalLink, GitPullRequest, Minus, RefreshCw, Search, XCircle } from "lucide-react"
import { Bar, BarChart, XAxis } from "recharts"
import {
  fetchCostOverview,
  fetchGeneralStats,
  fetchTaskRunAnalyticsSummary,
  fetchTaskRunAttempts,
  fetchTaskRunEvents,
  fetchTaskRunFilterOptions,
  fetchTaskRunPRs,
  fetchTaskRuns,
} from "@/lib/api"
import type {
  CostOverview,
  GeneralStats,
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
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart"
import { RunLogsDialog } from "@/components/run-logs-dialog"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"

const costChartConfig = {
  // Spend is a money series, so it takes the data hue, never an outcome color.
  costUsd: { label: "Cost", color: "var(--color-data)" },
} satisfies ChartConfig

export type DetailState = {
  attempts: TaskRunAttempt[]
  events: TaskRunEvent[]
  prs: TaskRunPR[]
}

type Cancellation = {
  cancelled: boolean
}

const anyValue = "__any__"
// Stable no-op subscribe for useSyncExternalStore-based environment checks.
const emptySubscribe = () => () => {}
const DEFAULT_PAGE_LIMIT = 50
const MAX_DISPLAYED_EVENTS = 12

export const urlFilterKeys = [
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
type KpiFilter = "runs" | "clean" | "humanInTheLoop" | "warning" | "failed" | "humanTouches" | "mergedPrs"

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
  const [costOverview, setCostOverview] = useState<CostOverview | null>(null)
  const [generalStats, setGeneralStats] = useState<GeneralStats | null>(null)
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
  const loadCancellationRef = useRef<Cancellation | null>(null)

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

      const overviewFilters = { ...filters, cursor: undefined }
      const [kpiSummaryData, runsData, optionsData, costData, statsData] = await Promise.all([
        fetchTaskRunAnalyticsSummary(kpiFilters),
        fetchTaskRuns(query),
        optionsRef.current ? Promise.resolve(optionsRef.current) : fetchTaskRunFilterOptions(),
        // Cost and general-stats failures must not break the main load.
        fetchCostOverview(overviewFilters).catch(() => null),
        fetchGeneralStats(overviewFilters).catch(() => null),
      ])
      if (cancellation?.cancelled) return
      setKpiSummary(kpiSummaryData)
      setCostOverview(costData)
      setGeneralStats(statsData)
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
    loadCancellationRef.current = cancellation
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
    if (kpi === "clean") updates.status = "clean"
    if (kpi === "humanInTheLoop") updates.status = "human_in_the_loop"
    if (kpi === "warning") updates.status = "warning"
    if (kpi === "failed") updates.status = "failed"
    if (kpi === "humanTouches") updates.humanTouched = true
    if (kpi === "mergedPrs") updates.mergedPrs = true
    replaceFilterParams(updates)
  }

  const hasCostChartData = useMemo(
    () => (costOverview?.dailySeries ?? []).some((point) => point.costUsd > 0),
    [costOverview]
  )

  const ignoredCostFilters = useMemo(() => {
    const labels: string[] = []
    if (filters.repo) labels.push("repo")
    if (filters.status) labels.push("status")
    if (filters.warningType) labels.push("warning")
    if (filters.failureType) labels.push("failure")
    if (filters.humanTouched !== undefined) labels.push("human touched")
    if (filters.mergedPrs !== undefined) labels.push("merged PRs")
    return labels
  }, [filters.repo, filters.status, filters.warningType, filters.failureType, filters.humanTouched, filters.mergedPrs])

  const activeKpi = useMemo<KpiFilter | null>(() => {
    if (filters.humanTouched === true) return "humanTouches"
    if (filters.mergedPrs === true) return "mergedPrs"
    if (filters.status === "clean") return "clean"
    if (filters.status === "human_in_the_loop") return "humanInTheLoop"
    if (filters.status === "warning") return "warning"
    if (filters.status === "failed") return "failed"
    if (!filters.status && filters.humanTouched === undefined && filters.mergedPrs === undefined) return "runs"
    return null
  }, [filters.humanTouched, filters.mergedPrs, filters.status])

  return (
    <main className="flex h-full min-w-0 bg-background">
      <section className="flex min-w-0 flex-1 flex-col">
        <header className="flex flex-col gap-3 border-b-2 border-border px-4 py-3 lg:px-6">
          <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <div>
              <h2 className="text-lg tracking-tight">Task Run Analytics</h2>
              <p className="mt-0.5 text-xs text-muted-foreground">PR-scoped run outcomes, warnings, and delivery failures.</p>
            </div>
            <div className="flex w-fit items-center gap-2">
              <Button variant="ghost" size="sm" onClick={clearFilters}>Reset</Button>
              <Button variant="outline" size="sm" className="gap-2" onClick={() => load()}>
                <RefreshCw className={cn("size-4", loading && "animate-spin")} />
                Refresh
              </Button>
            </div>
          </div>
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
              <h3 className="kicker font-semibold text-muted-foreground">AI Spend</h3>
              {ignoredCostFilters.length > 0 && (
                <span className="text-xs italic text-muted-foreground">Ignores {formatFilterList(ignoredCostFilters)} filters</span>
              )}
            </div>
            <div className={cn("grid gap-2 sm:grid-cols-2 xl:grid-cols-4", ignoredCostFilters.length > 0 && "opacity-75")}>
              <CostCard
                label="Today's Cost"
                value={
                  <>
                    <span>{formatUSD(costOverview?.today.costUsd)}</span>
                    {costOverview?.today.deltaPctVsYesterday != null && (
                      <DeltaIndicator pct={costOverview.today.deltaPctVsYesterday} />
                    )}
                  </>
                }
              />
              <CostCard label="Weekly Cost" value={formatUSD(costOverview?.week.costUsd)} />
              <CostCard label="Monthly Cost" value={formatUSD(costOverview?.month.costUsd)} />
              <CostCard
                label="Projected Monthly Cost"
                value={
                  <>
                    <span>{formatUSD(costOverview?.projectedMonth?.costUsd)}</span>
                    {costOverview?.projectedMonth && (
                      <ConfidenceBadge confidence={costOverview.projectedMonth.confidence} />
                    )}
                  </>
                }
                sub={costOverview?.projectedMonth ? "based on current daily average" : undefined}
              />
            </div>
            {costOverview && hasCostChartData && (
              <ChartContainer config={costChartConfig} className="aspect-auto h-24 w-full">
                <BarChart data={costOverview.dailySeries} margin={{ top: 4, right: 4, bottom: 0, left: 4 }}>
                  <XAxis
                    dataKey="date"
                    tickLine={false}
                    axisLine={false}
                    tickMargin={6}
                    minTickGap={24}
                    tickFormatter={formatChartDate}
                  />
                  <ChartTooltip
                    cursor={false}
                    content={
                      <ChartTooltipContent
                        hideIndicator
                        labelFormatter={(value) => formatChartDate(value as string)}
                        formatter={(value) => formatUSD(value as number)}
                      />
                    }
                  />
                  <Bar dataKey="costUsd" fill="var(--color-costUsd)" radius={2} />
                </BarChart>
              </ChartContainer>
            )}
          </div>
          {generalStats && (
            <div className="flex flex-col gap-2">
              <h3 className="kicker font-semibold text-muted-foreground">General Stats</h3>
              <div className="grid gap-2 sm:grid-cols-3">
                <CostCard
                  label="Avg ticket to PR"
                  value={formatDurationMs(generalStats.ticketToPrMs.avgMs)}
                  sub={
                    generalStats.ticketToPrMs.samples > 0 &&
                    typeof generalStats.ticketToPrMs.authoritativeSamples === "number" &&
                    generalStats.ticketToPrMs.authoritativeSamples < generalStats.ticketToPrMs.samples
                      ? `${generalStats.ticketToPrMs.authoritativeSamples} of ${generalStats.ticketToPrMs.samples} samples use real ticket creation time`
                      : undefined
                  }
                />
                <CostCard label="Avg ready→merge" title="Business hours from ready-for-review to merge, in your local timezone." value={formatDurationMs(generalStats.prOpenToMergeMs.avgMs)} />
                <CostCard label="Avg AI implementation time" value={formatDurationMs(generalStats.aiImplMs.avgMs)} />
              </div>
            </div>
          )}
          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-7">
            <Metric label="Runs" value={kpiSummary?.totalRuns ?? 0} active={activeKpi === "runs"} onClick={() => applyKpiFilter("runs")} />
            <Metric label="Clean" title="PR merged or closed with zero human interaction." value={kpiSummary?.byStatus.clean ?? 0} tone="success" active={activeKpi === "clean"} onClick={() => applyKpiFilter("clean")} />
            <Metric label="Human on the loop" title="PR merged or closed; a human interacted via the PR (comment, review, or code push)." value={kpiSummary?.byStatus.human_in_the_loop ?? 0} active={activeKpi === "humanInTheLoop"} onClick={() => applyKpiFilter("humanInTheLoop")} />
            <Metric label="Warning" title="PR merged or closed; a human interacted via the factory dashboard." value={kpiSummary?.byStatus.warning ?? 0} tone="warning" active={activeKpi === "warning"} onClick={() => applyKpiFilter("warning")} />
            <Metric label="Failed" title="No PR was ever delivered or the run definitively failed before delivery." value={kpiSummary?.byStatus.failed ?? 0} tone="danger" active={activeKpi === "failed"} onClick={() => applyKpiFilter("failed")} />
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
          <div className="overflow-hidden rounded-lg border border-border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[140px]">Status</TableHead>
                  <TableHead>Owner</TableHead>
                  <TableHead>Ticket</TableHead>
                  <TableHead>Repo</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Warnings</TableHead>
                  <TableHead className="text-right">Cost</TableHead>
                  <TableHead className="text-right">Tokens</TableHead>
                  <TableHead className="text-right">Duration</TableHead>
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
                        <div className="font-mono font-medium">{run.ownerDisplayName || run.factoryName || run.ownerType}</div>
                        <div className="text-[11px] text-muted-foreground">{ownerSecondaryLine(run)}</div>
                      </div>
                    </TableCell>
                    <TableCell>
                      {run.issueId ? (
                        <span className="block max-w-[200px] truncate font-mono">
                          {run.issueTitle ? `${run.issueId}: ${run.issueTitle}` : run.issueId}
                        </span>
                      ) : (
                        <span className="text-muted-foreground">None</span>
                      )}
                    </TableCell>
                    <TableCell className="font-mono">{run.repo || <span className="font-sans text-muted-foreground">None</span>}</TableCell>
                    <TableCell>{run.model || <span className="text-muted-foreground">Unknown</span>}</TableCell>
                    <TableCell>
                      <div className="flex max-w-[260px] flex-wrap gap-1">
                        {run.warningTypes.length === 0 ? (
                          <span className="text-xs text-muted-foreground">None</span>
                        ) : run.warningTypes.map((warning) => <Badge key={warning} variant="outline">{formatLabel(warning)}</Badge>)}
                      </div>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{formatUSD(run.estimatedCostUsd)}</TableCell>
                    <TableCell className="text-right tabular-nums text-muted-foreground">{formatCompactTokens(run.totalTokens)}</TableCell>
                    <TableCell className="text-right tabular-nums text-muted-foreground">{formatDurationMs(runDurationMs(run))}</TableCell>
                    <TableCell className="text-right">{run.mergedPrCount}/{run.prCount}</TableCell>
                    <TableCell className="text-right text-muted-foreground">{formatTime(run.startedAt)}</TableCell>
                  </TableRow>
                ))}
                {!loading && runs.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={11} className="h-32 text-center text-muted-foreground">
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
              <Button variant="outline" size="sm" onClick={() => load(nextCursor, true, loadCancellationRef.current ?? undefined)} disabled={loading}>
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

function Metric({ label, title, value, tone, active, onClick }: { label: string; title?: string; value: number; tone?: "success" | "warning" | "danger"; active?: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      title={title}
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "rounded-lg border border-border bg-card px-3 py-2 text-left transition-colors hover:bg-foreground/4 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
        active && "border-primary bg-foreground/7"
      )}
    >
      <div className="text-[11px] leading-[1.35] text-muted-foreground">{label}</div>
      <div className={cn(
        "mt-1 text-[23px] font-extrabold leading-none tracking-[-0.02em] tabular-nums",
        tone === "success" && "text-status-ok",
        tone === "warning" && "text-status-warn",
        tone === "danger" && "text-destructive"
      )}>{value}</div>
    </button>
  )
}

function CostCard({ label, value, sub, title }: { label: string; value: ReactNode; sub?: ReactNode; title?: string }) {
  return (
    <div className="rounded-lg border border-border bg-card px-3 py-2" title={title}>
      <div className="text-[11px] leading-[1.35] text-muted-foreground">{label}</div>
      <div className="mt-1 flex items-baseline gap-2 text-[23px] font-extrabold leading-none tracking-[-0.02em] tabular-nums">{value}</div>
      {sub && <div className="mt-1.5 text-[11px] text-muted-foreground">{sub}</div>}
    </div>
  )
}

function DeltaIndicator({ pct }: { pct: number }) {
  const flat = pct === 0
  const up = pct > 0
  const Icon = flat ? Minus : up ? ArrowUp : ArrowDown
  return (
    <span
      className={cn(
        "flex items-center gap-0.5 text-[11px] font-semibold",
        // A spend increase is worse (accent); a decrease is better (status-ok); flat is neutral.
        flat ? "text-muted-foreground" : up ? "text-primary" : "text-status-ok"
      )}
    >
      <Icon className="size-3" />
      {Math.abs(pct).toFixed(1)}%
    </span>
  )
}

function ConfidenceBadge({ confidence }: { confidence: "high" | "medium" | "low" }) {
  return (
    <Badge
      variant="outline"
      className={cn(
        "font-normal",
        confidence === "high" && "border-status-ok text-status-ok",
        confidence === "medium" && "border-status-warn text-status-warn",
        confidence === "low" && "border-border text-muted-foreground"
      )}
    >
      {formatLabel(confidence)}
    </Badge>
  )
}

export function FilterSelect({ label, value, values, onChange }: { label: string; value?: string; values?: string[]; onChange: (value?: string) => void }) {
  const selectValues = value && !(values ?? []).includes(value)
    ? [value, ...(values ?? [])]
    : (values ?? [])
  const displayValue = value ? formatLabel(value) : "any"

  return (
    <Select value={value ?? anyValue} onValueChange={(next) => onChange(next === anyValue ? undefined : next)}>
      <SelectTrigger size="sm" className="w-full text-[13px]">
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

export function RunDetailPanel({ run, details, loading, error, onClose }: { run: TaskRunSummary | null; details: DetailState | null; loading: boolean; error: string | null; onClose: () => void }) {
  // The portal target (document.body) only exists on the client, so render
  // nothing during SSR/hydration. useSyncExternalStore is the recommended way
  // to read client-only environment state without a setState-in-effect flip.
  const mounted = useSyncExternalStore(
    emptySubscribe,
    () => true,
    () => false
  )

  if (!run || !mounted) return null
  return createPortal(
    <>
    <div className="fixed inset-0 z-[55] bg-[var(--ds-scrim)]" onClick={onClose} aria-hidden="true" />
    <aside className="fixed inset-y-0 right-0 z-[60] flex w-full max-w-[66vw] flex-col border-l-2 border-border bg-card shadow-ds-lg">
      <div className="flex items-start justify-between gap-3 border-b-2 border-border p-4">
        <div className="min-w-0">
          <div className="truncate text-[15px] font-extrabold tracking-[-0.015em]">{run.ownerDisplayName || run.runId}</div>
          <div className="truncate font-mono text-xs text-muted-foreground">{run.runId}</div>
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
        <RunLogsDialog key={run.runId} run={run} attempts={details?.attempts ?? []} />
        <section className="grid grid-cols-2 gap-2 text-sm">
          <DetailItem label="Status" value={<StatusBadge status={run.status} />} />
          <DetailItem label="Phase" value={formatLabel(run.phase)} />
          <DetailItem label="Issue" value={run.issueId ? (run.issueTitle ? `${run.issueId}: ${run.issueTitle}` : run.issueId) : "None"} />
          <DetailItem label="Workflow" value={run.workflowName || "None"} />
          <DetailItem label="Started" value={formatTime(run.startedAt)} />
          <DetailItem label="Duration" value={formatDurationMs(runDurationMs(run))} />
          <DetailItem label="Failure" value={run.failureType ? formatLabel(run.failureType) : "None"} />
          <DetailItem label="Human" value={String(run.humanInteractionCount)} />
        </section>
        {hasUsageData(run) && (
          <section>
            <h3 className="kicker mb-2 font-semibold text-muted-foreground">Usage</h3>
            <div className="grid grid-cols-2 gap-2 text-sm">
              <DetailItem label="Prompt tokens" value={formatCompactTokens(run.inputTokens)} />
              <DetailItem label="Completion tokens" value={formatCompactTokens(run.outputTokens)} />
              <DetailItem label="Total tokens" value={formatCompactTokens(run.totalTokens)} />
              <DetailItem label="Estimated cost" value={formatUSD(run.estimatedCostUsd)} />
              <DetailItem label="Model" value={run.model || "Unknown"} />
            </div>
          </section>
        )}
        <TimingSection run={run} />
        <section>
          <h3 className="kicker mb-2 font-semibold text-muted-foreground">Pull Requests</h3>
          <div className="space-y-2">
            {(details?.prs ?? []).map((pr) => (
              <a key={pr.id} href={pr.url} target="_blank" rel="noreferrer" className="flex items-center justify-between rounded-md border border-border px-3 py-2 text-sm hover:bg-foreground/7">
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
          <h3 className="kicker mb-2 font-semibold text-muted-foreground">Attempts</h3>
          <div className="space-y-2">
            {(details?.attempts ?? []).map((attempt) => {
              const attemptDurationMs = durationBetween(attempt.startedAt, attempt.finishedAt)
              return (
                <div key={attempt.id} className="rounded-md border border-border px-3 py-2 text-sm">
                  <div className="flex items-center justify-between">
                    <span>Attempt {attempt.attemptNumber}</span>
                    <Badge variant="outline">{attempt.status}</Badge>
                  </div>
                  {attemptDurationMs !== undefined && (
                    <div className="mt-1 text-xs text-muted-foreground">{formatDurationMs(attemptDurationMs)}</div>
                  )}
                  {attempt.failureType && <div className="mt-1 text-xs text-muted-foreground">{formatLabel(attempt.failureType)}</div>}
                </div>
              )
            })}
          </div>
        </section>
        <section>
          <h3 className="kicker mb-2 font-semibold text-muted-foreground">Events</h3>
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
    </>,
    document.body,
  )
}

function TimingSection({ run }: { run: TaskRunSummary }) {
  const phases = runTimingPhases(run)
  if (phases.length === 0) return null
  return (
    <section>
      <h3 className="kicker mb-2 font-semibold text-muted-foreground">Timing</h3>
      <div className="grid grid-cols-2 gap-2 text-sm">
        {phases.map((phase) => (
          <DetailItem key={phase.label} label={phase.label} value={formatDurationMs(phase.ms)} title={phase.title} />
        ))}
      </div>
    </section>
  )
}

function DetailItem({ label, value, title }: { label: string; value: ReactNode; title?: string }) {
  return (
    <div title={title} className="rounded-md border border-border px-3 py-2">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div className="mt-1 truncate">{value}</div>
    </div>
  )
}

// A run status reads as its outcome color — the same semantic scale the charts
// use — drawn as an outlined pill. Failed is the one outcome that also takes a
// soft accent fill, so a broken run is findable at a glance in a long table.
// Full literal class strings: Tailwind only sees classes it can read verbatim.
const statusPillClasses: Record<string, string> = {
  clean: "border-status-ok text-status-ok",
  human_in_the_loop: "border-[var(--ds-neutral-500)] text-[var(--ds-neutral-500)]",
  warning: "border-status-warn text-status-warn",
  failed: "border-primary bg-tint-accent text-primary",
  running: "border-data text-data",
}

export function StatusBadge({ status }: { status: string }) {
  return (
    <Badge
      title={status === "clean" ? "PR merged or closed with zero human interaction." : status === "human_in_the_loop" ? "PR merged or closed; a human interacted via the PR (comment, review, or code push)." : status === "warning" ? "PR merged or closed; a human interacted via the factory dashboard." : status === "failed" ? "No PR was ever delivered or the run definitively failed before delivery." : status === "running" ? "In progress; no failure has occurred." : undefined}
      variant="outline"
      className={cn("gap-1.5 pl-2 tracking-[0.02em]", statusPillClasses[status] ?? "border-border text-muted-foreground")}
    >
      {/* The outcome reads from the dot and the ring; the label itself stays in
          the normal text color so a row of pills isn't a row of colored words. */}
      <span aria-hidden className="size-[7px] shrink-0 rounded-full bg-current" />
      <span className="text-foreground">{formatLabel(status)}</span>
    </Badge>
  )
}

function hasUsageData(run: TaskRunSummary) {
  return run.inputTokens !== undefined || run.outputTokens !== undefined || run.totalTokens !== undefined || run.estimatedCostUsd !== undefined
}

// Pinned to "en" so the conjunction matches the English filter labels and SSR/client output agree.
const filterListFormatter = new Intl.ListFormat("en", { style: "long", type: "conjunction" })

function formatFilterList(labels: string[]) {
  return filterListFormatter.format(labels)
}

export function formatLabel(value: string) {
  return value.replace(/_/g, " ").replace(/\b\w/g, (match) => match.toUpperCase())
}

const usdFormatter = new Intl.NumberFormat(undefined, {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
})

function formatUSD(value: number | null | undefined) {
  if (value === null || value === undefined || Number.isNaN(value)) return "—"
  return usdFormatter.format(value)
}

const compactTokenFormatter = new Intl.NumberFormat(undefined, {
  notation: "compact",
  maximumFractionDigits: 1,
})

function formatCompactTokens(value: number | null | undefined) {
  if (value === null || value === undefined || Number.isNaN(value)) return "—"
  return compactTokenFormatter.format(value)
}

function formatDurationMs(ms: number | null | undefined) {
  if (ms === null || ms === undefined || Number.isNaN(ms)) return "—"
  const totalSeconds = Math.max(0, Math.round(ms / 1000))
  if (totalSeconds < 60) return `${totalSeconds}s`
  const totalMinutes = Math.floor(totalSeconds / 60)
  if (totalMinutes < 60) {
    const seconds = totalSeconds % 60
    return seconds ? `${totalMinutes}m ${seconds}s` : `${totalMinutes}m`
  }
  const totalHours = Math.floor(totalMinutes / 60)
  if (totalHours < 24) {
    const minutes = totalMinutes % 60
    return minutes ? `${totalHours}h ${minutes}m` : `${totalHours}h`
  }
  const days = Math.floor(totalHours / 24)
  const hours = totalHours % 24
  return hours ? `${days}d ${hours}h` : `${days}d`
}

function durationBetween(start: number | null | undefined, end: number | null | undefined) {
  if (!start || !end) return undefined
  const ms = end - start
  return ms >= 0 ? ms : undefined
}

function runDurationMs(run: TaskRunSummary) {
  return durationBetween(run.startedAt, run.finishedAt)
}

function ownerSecondaryLine(run: TaskRunSummary) {
  const parts = [run.workflowName, run.workspaceName || run.integration].filter(Boolean)
  return parts.length > 0 ? parts.join(" · ") : "unknown"
}

function runTimingPhases(run: TaskRunSummary) {
  const phases: { label: string; ms: number; title: string }[] = []
  const push = (label: string, ms: number | undefined, title: string) => {
    if (ms !== undefined) phases.push({ label, ms, title })
  }
  push("Queued wait", durationBetween(run.queuedAt, run.agentStartedAt), "Time between the run being queued and the agent starting to work (covers provisioning and startup).")
  push("Implementation", durationBetween(run.agentStartedAt, run.prOpenedAt), "Time from the agent starting to the PR being opened.")
  // 0 is a real value here (a merge entirely outside business hours), so the
  // row is driven by the run having merged rather than by the duration.
  push("Ready to merge", run.mergedAt > 0 ? (run.readyToMergeMs ?? 0) : undefined, "Business-hours time in your timezone from ready-for-review to merge.")
  return phases
}

function formatChartDate(value: string) {
  // Daily series dates are "YYYY-MM-DD"; render a short month/day tick.
  const parts = value.split("-").map(Number)
  if (parts.length !== 3 || parts.some(Number.isNaN)) return value
  const [year, month, day] = parts
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(new Date(year, month - 1, day))
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
