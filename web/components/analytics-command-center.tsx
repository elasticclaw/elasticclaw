"use client"

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { ChevronRight } from "lucide-react"
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ReferenceLine,
  XAxis,
  YAxis,
} from "recharts"
import {
  fetchAnalyticsCostDrivers,
  fetchAnalyticsCosts,
  fetchAnalyticsEffectiveness,
  fetchGeneralStats,
  fetchTaskRunAnalyticsSummary,
  fetchTaskRunAttempts,
  fetchTaskRunEvents,
  fetchTaskRunFilterOptions,
  fetchTaskRunPRs,
  fetchTaskRuns,
} from "@/lib/api"
import type {
  AnalyticsCostDriver,
  AnalyticsEffectiveness,
  CostOverview,
  GeneralStats,
  TaskRunAnalyticsFilters,
  TaskRunAnalyticsSummary,
  TaskRunFilterOptions,
  TaskRunSummary,
} from "@/lib/types"
import {
  FilterSelect,
  RunDetailPanel,
  StatusBadge,
  type DetailState,
  urlFilterKeys,
} from "@/components/task-run-analytics-view"
import { Button } from "@/components/ui/button"
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

const commandCenterUrlFilterKeys = ["workspace", ...urlFilterKeys, "from", "to"] as const
const chartConfig = {
  clean: { label: "Clean", color: "#0ca30c" },
  warning: { label: "Warning", color: "#fab219" },
  failed: { label: "Failed", color: "#d03b3b" },
  costPerMergedPr: { label: "Cost / merged PR", color: "var(--chart-1)" },
} satisfies ChartConfig
const usd = new Intl.NumberFormat(undefined, {
  style: "currency",
  currency: "USD",
  maximumFractionDigits: 2,
})
const usdWhole = new Intl.NumberFormat(undefined, {
  style: "currency",
  currency: "USD",
  maximumFractionDigits: 0,
})

function formatDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(
    new Date(`${value}T00:00:00`)
  )
}

function formatDuration(milliseconds?: number | null) {
  if (!milliseconds) return "—"
  return milliseconds < 3_600_000
    ? `${Math.round(milliseconds / 60_000)}m`
    : `${(milliseconds / 3_600_000).toFixed(1)}h`
}

function formatPercent(value?: number) {
  return value == null ? "—" : `${(value * 100).toFixed(1)}%`
}

function isoDate(date: Date) {
  return date.toISOString().slice(0, 10)
}

function isoDayRange(day: string) {
  return {
    from: new Date(`${day}T00:00:00.000Z`).toISOString(),
    to: new Date(`${day}T23:59:59.999Z`).toISOString(),
  }
}

export function AnalyticsCommandCenter() {
  const router = useRouter()
  const pathname = usePathname()
  const params = useSearchParams()
  const paramsKey = params.toString()
  const filters = useMemo(() => {
    const nextFilters: TaskRunAnalyticsFilters = {
      analyticsEnabled: true,
      requiresPr: true,
      limit: 50,
    }

    for (const filterKey of commandCenterUrlFilterKeys) {
      const value = params.get(filterKey)
      if (value) (nextFilters as Record<string, string>)[filterKey] = value
    }

    return nextFilters
  }, [params, paramsKey])
  const [summary, setSummary] = useState<TaskRunAnalyticsSummary>()
  const [costs, setCosts] = useState<CostOverview>()
  const [yearCosts, setYearCosts] = useState<CostOverview>()
  const [effect, setEffect] = useState<AnalyticsEffectiveness>()
  const [stats, setStats] = useState<GeneralStats>()
  const [drivers, setDrivers] = useState<AnalyticsCostDriver[]>([])
  const [runs, setRuns] = useState<TaskRunSummary[]>([])
  const [options, setOptions] = useState<TaskRunFilterOptions>()
  const [nextCursor, setNextCursor] = useState<string>()
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)
  const [details, setDetails] = useState<DetailState | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [error, setError] = useState<string>()
  const loadAbortController = useRef<AbortController | null>(null)
  const loadRequestId = useRef(0)

  const selectedRun = useMemo(
    () => runs.find((run) => run.runId === selectedRunId) ?? null,
    [runs, selectedRunId]
  )

  const setFilters = useCallback(
    (updates: Record<string, string | undefined>) => {
      const nextParams = new URLSearchParams(paramsKey)
      Object.entries(updates).forEach(([filterKey, value]) => {
        if (value) nextParams.set(filterKey, value)
        else nextParams.delete(filterKey)
      })
      nextParams.delete("cursor")
      router.replace(`${pathname}${nextParams.size ? `?${nextParams}` : ""}`, { scroll: false })
    },
    [paramsKey, pathname, router]
  )

  const load = useCallback(
    async (cursor?: string, append = false) => {
      loadAbortController.current?.abort()
      const controller = new AbortController()
      loadAbortController.current = controller
      const requestId = ++loadRequestId.current
      try {
        setError(undefined)
        // Default to the last 30 days so every widget reads the same period.
        // Computed here (not in render) — reading the clock during render
        // suspends the prerender under Next's cacheComponents semantics.
        const effectiveFilters = { ...filters }
        if (!effectiveFilters.from && !effectiveFilters.to) {
          const to = new Date()
          const from = new Date(to)
          from.setDate(to.getDate() - 30)
          effectiveFilters.from = from.toISOString()
          effectiveFilters.to = to.toISOString()
        }
        const runFilters = { ...effectiveFilters, cursor }
        const [
          summaryData,
          costsData,
          yearCostsData,
          effectData,
          statsData,
          driversData,
          runsData,
          optionsData,
        ] = await Promise.all([
          fetchTaskRunAnalyticsSummary(effectiveFilters, { signal: controller.signal }),
          fetchAnalyticsCosts(effectiveFilters, 30, "model", { signal: controller.signal }),
          // The year heatmap always shows the trailing year, regardless of the
          // selected period. It also drops the run-level flags so the backend
          // serves the full usage ledger (usage_daily) instead of restricting
          // cost to days that still have task runs.
          fetchAnalyticsCosts(
            { ...effectiveFilters, from: undefined, to: undefined, analyticsEnabled: undefined, requiresPr: undefined },
            366,
            undefined,
            { signal: controller.signal }
          ),
          fetchAnalyticsEffectiveness(effectiveFilters, { signal: controller.signal }),
          fetchGeneralStats(effectiveFilters, { signal: controller.signal }),
          fetchAnalyticsCostDrivers(effectiveFilters, "factory", { signal: controller.signal }),
          fetchTaskRuns(runFilters, { signal: controller.signal }),
          options ? Promise.resolve(options) : fetchTaskRunFilterOptions({ signal: controller.signal }),
        ])
        if (controller.signal.aborted || requestId !== loadRequestId.current) return

        setSummary(summaryData)
        setCosts(costsData)
        setYearCosts(yearCostsData)
        setEffect(effectData)
        setStats(statsData)
        setDrivers(driversData)
        setRuns((currentRuns) => (append ? [...currentRuns, ...runsData.runs] : runsData.runs))
        setNextCursor(runsData.nextCursor)
        setOptions(optionsData)
        if (!append) {
          setSelectedRunId(null)
          setDetails(null)
          setDetailError(null)
        }
      } catch (loadError) {
        if (controller.signal.aborted || (loadError instanceof Error && loadError.name === "AbortError")) return
        if (requestId !== loadRequestId.current) return
        setError(loadError instanceof Error ? loadError.message : "Unable to load analytics")
      }
    },
    [filters, options]
  )

  useEffect(() => {
    queueMicrotask(() => void load())
    return () => {
      loadAbortController.current?.abort()
    }
  }, [load])

  useEffect(() => {
    if (!selectedRunId) return

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
          if (!cancelled) setDetails({ attempts: attempts.attempts, events: events.events, prs: prs.prs })
        })
        .catch((detailLoadError) => {
          if (!cancelled) {
            setDetailError(
              detailLoadError instanceof Error ? detailLoadError.message : "Unable to load run details"
            )
          }
        })
        .finally(() => {
          if (!cancelled) setDetailLoading(false)
        })
    })

    return () => {
      cancelled = true
    }
  }, [selectedRunId])

  const totalCost = costs?.dailySeries.reduce((sum, point) => sum + point.costUsd, 0) ?? 0
  const priorCost = costs?.priorPeriodCostUsd ?? costs?.prior?.periodCostUsd
  const costDelta = calculateDelta(totalCost, priorCost)
  const heatmap = useHeatmap(yearCosts)
  const maxHeatCost = Math.max(...heatmap.days.map((day) => day.point?.costUsd ?? 0), 0)
  const modelData = useModelData(costs)

  return (
    <main className="h-full overflow-auto bg-background">
      <div className="mx-auto max-w-[1400px] space-y-5 p-5 lg:p-6">
        <header>
          <h1 className="text-2xl font-semibold tracking-tight">Analytics</h1>
        </header>
        <FilterBar filters={filters} options={options} onChange={setFilters} />
        {error && <p className="text-sm text-destructive">{error}</p>}

        <div className="grid gap-5 xl:grid-cols-[5fr_2fr]">
          <KpiGroup title="Effectiveness">
            <Kpi
              label="Runs"
              value={summary?.totalRuns}
              good
              change={calculateDelta(summary?.totalRuns, summary?.prior?.totalRuns)}
              onClick={() => setFilters({ status: undefined })}
            />
            <Kpi
              label="Success rate"
              value={formatPercent(effect?.successRate)}
              good
              onClick={() => setFilters({ status: "clean_success" })}
            />
            <Kpi label="Merge rate" value={formatPercent(effect?.mergeRate)} good />
            <Kpi
              label="Avg ticket to PR ready for review"
              value={formatDuration(stats?.ticketToPrMs.avgMs)}
              change={calculateDelta(stats?.ticketToPrMs.avgMs, stats?.prior?.ticketToPrMs.avgMs)}
            />
            <Kpi
              label="Avg PR ready for review to merged"
              value={formatDuration(stats?.prOpenToMergeMs.avgMs)}
              change={calculateDelta(stats?.prOpenToMergeMs.avgMs, stats?.prior?.prOpenToMergeMs.avgMs)}
            />
          </KpiGroup>
          <KpiGroup title="Cost">
            <Kpi label="Total cost" value={usdWhole.format(totalCost)} change={costDelta} cost />
            <Kpi
              label="Cost per run"
              value={usd.format(summary?.totalRuns ? totalCost / summary.totalRuns : 0)}
              cost
            />
          </KpiGroup>
        </div>

        <Heatmap
          heatmap={heatmap}
          maxCost={maxHeatCost}
          onSelectDay={(day) => setFilters(isoDayRange(day))}
        />

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Daily cost by model">
            <DailyCostChart costs={costs} modelData={modelData} />
          </ChartCard>
          <ChartCard title="Run outcomes over time">
            <OutcomesChart effect={effect} />
          </ChartCard>
          <ChartCard title="Delivery funnel">
            <DeliveryFunnel effect={effect} />
          </ChartCard>
          <ChartCard title="Cost per merged PR">
            <CostPerMergedPrChart effect={effect} />
          </ChartCard>
        </div>

        <CostDrivers drivers={drivers} onSelect={(factory) => setFilters({ factory })} />
        <RunsTable
          runs={runs}
          nextCursor={nextCursor}
          onSelect={setSelectedRunId}
          onLoadMore={() => void load(nextCursor, true)}
        />
      </div>
      <RunDetailPanel
        run={selectedRun}
        details={details}
        loading={detailLoading}
        error={detailError}
        onClose={() => setSelectedRunId(null)}
      />
    </main>
  )
}

function FilterBar({
  filters,
  options,
  onChange,
}: {
  filters: TaskRunAnalyticsFilters
  options?: TaskRunFilterOptions
  onChange: (updates: Record<string, string | undefined>) => void
}) {
  const selectFilters = [
    ["Factory", "factory", options?.factories],
    ["Workflow", "workflow", options?.workflows],
    ["Repo", "repo", options?.repos],
    ["Model", "model", options?.models],
  ] as const

  return (
    <div className="flex flex-wrap gap-2 rounded-lg border bg-card p-2">
      <div className="flex overflow-hidden rounded-md border">
        {[["7d", 7], ["30d", 30], ["90d", 90], ["MTD", 0]].map(([label, days]) => (
          <Button
            key={label}
            variant="ghost"
            size="sm"
            className="rounded-none"
            onClick={() => {
              const to = new Date()
              const from = new Date()
              if (days) from.setDate(to.getDate() - Number(days))
              else from.setDate(1)
              onChange({ from: from.toISOString(), to: to.toISOString() })
            }}
          >
            {label}
          </Button>
        ))}
      </div>
      {selectFilters.map(([label, key, values]) => (
        <div key={key} className="w-48">
          <FilterSelect
            label={label}
            value={filters[key]}
            values={values}
            onChange={(value) => onChange({ [key]: value })}
          />
        </div>
      ))}
    </div>
  )
}

function useHeatmap(costs?: CostOverview) {
  return useMemo(() => {
    const series = costs?.dailySeries ?? []
    // Anchor the grid on the data's last day rather than the wall clock —
    // reading the clock during render suspends the prerender under Next's
    // cacheComponents semantics (and the series' last day IS "today" as far
    // as the backend is concerned).
    if (series.length === 0) return { days: [], monthLabels: [] }
    const costsByDate = new Map(series.map((point) => [point.date, point]))
    const end = new Date(`${series[series.length - 1].date}T00:00:00`)
    const start = new Date(end); start.setDate(start.getDate() - 363 - ((start.getDay() + 6) % 7))
    const days = Array.from({ length: 364 }, (_, index) => { const date = new Date(start); date.setDate(date.getDate() + index); const iso = isoDate(date); return { iso, point: costsByDate.get(iso), week: Math.floor(index / 7), day: index % 7 } })
    const monthLabels = days.filter((day, index) => index === 0 || day.iso.slice(5, 7) !== days[index - 1].iso.slice(5, 7)).map((day) => ({ week: day.week, label: new Intl.DateTimeFormat(undefined, { month: "short" }).format(new Date(`${day.iso}T00:00:00`)) }))
    return { days, monthLabels }
  }, [costs])
}

function Heatmap({ heatmap, maxCost, onSelectDay }: { heatmap: ReturnType<typeof useHeatmap>; maxCost: number; onSelectDay: (day: string) => void }) {
  return <section className="rounded-lg border bg-card p-4"><h2 className="mb-4 text-sm font-semibold">Cost by day</h2><div className="grid grid-cols-[24px_1fr] gap-2"><div className="pt-5 text-[10px] text-muted-foreground"><div className="grid grid-rows-7"><span /><span>Mon</span><span /><span>Wed</span><span /><span>Fri</span><span /></div></div><div><div className="relative mb-1 h-4 text-[10px] text-muted-foreground">{heatmap.monthLabels.map(({ week, label }) => <span key={`${week}-${label}`} className="absolute" style={{ left: `${(week / 52) * 100}%` }}>{label}</span>)}</div><div className="grid grid-flow-col grid-cols-[repeat(52,minmax(0,1fr))] grid-rows-7 gap-1">{heatmap.days.map(({ iso, point }) => { const level = point?.costUsd ? Math.min(5, Math.ceil((point.costUsd / maxCost) * 5)) : 0; return <button key={iso} title={`${iso}: ${usd.format(point?.costUsd ?? 0)} · ${point?.runCount ?? 0} runs`} onClick={() => onSelectDay(iso)} className="aspect-square rounded-sm border border-black/5" style={{ background: level ? `var(--heatmap-${level})` : "var(--muted)" }} /> })}</div></div></div><div className="mt-3 flex justify-end gap-1 text-xs text-muted-foreground">Less {Array.from({ length: 5 }, (_, index) => <i key={index} className="size-3 rounded-sm" style={{ background: `var(--heatmap-${index + 1})` }} />)} More</div></section>
}

function useModelData(costs?: CostOverview) { return useMemo(() => { const models = (costs?.seriesByModel ?? []).slice(0, 4); return (costs?.dailySeries ?? []).map((day, index) => ({ date: day.date, Other: Math.max(0, day.costUsd - models.reduce((sum, model) => sum + (model.dailySeries[index]?.costUsd ?? 0), 0)), ...Object.fromEntries(models.map((model) => [model.model, model.dailySeries[index]?.costUsd ?? 0])) })) }, [costs]) }
function DailyCostChart({ costs, modelData }: { costs?: CostOverview; modelData: Record<string, string | number>[] }) { const labels = [...(costs?.seriesByModel ?? []).slice(0, 4).map((item) => item.model), "Other"]; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={modelData}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><Legend />{labels.map((label, index) => <Bar key={label} dataKey={label} stackId="cost" fill={`var(--chart-${index + 1})`} />)}</BarChart></ChartContainer> }
function OutcomesChart({ effect }: { effect?: AnalyticsEffectiveness }) { return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect?.outcomesByDay}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis allowDecimals={false} /><ChartTooltip content={<ChartTooltipContent />} /><Legend /><Bar dataKey="clean" stackId="outcome" fill="var(--color-clean)" /><Bar dataKey="warning" stackId="outcome" fill="var(--color-warning)" /><Bar dataKey="failed" stackId="outcome" fill="var(--color-failed)" /></BarChart></ChartContainer> }
function DeliveryFunnel({ effect }: { effect?: AnalyticsEffectiveness }) { const stages = Object.entries(effect?.funnel ?? {}); return <div className="space-y-3 pt-3">{stages.map(([name, value], index) => { const previous = index ? Number(stages[index - 1][1]) : 0; return <div key={name}><div className="mb-1 flex justify-between text-sm"><span>{name.replace(/([A-Z])/g, " $1")}</span><span className="tabular-nums">{value} {index ? `(${formatPercent(previous ? Number(value) / previous : undefined)})` : ""}</span></div><div className="h-5 rounded bg-muted"><div className="h-full rounded bg-chart-1" style={{ width: `${(Number(value) / (effect?.funnel.started || 1)) * 100}%` }} /></div></div> })}</div> }
function CostPerMergedPrChart({ effect }: { effect?: AnalyticsEffectiveness }) { return <ChartContainer config={chartConfig} className="h-64 w-full"><LineChart data={effect?.costPerMergedPr.weekly}><CartesianGrid vertical={false} /><XAxis dataKey="weekStart" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><ReferenceLine y={effect?.costPerMergedPr.average} stroke="var(--muted-foreground)" /><Line type="monotone" dataKey="costPerMergedPr" stroke="var(--color-costPerMergedPr)" dot={false} /></LineChart></ChartContainer> }
function CostDrivers({ drivers, onSelect }: { drivers: AnalyticsCostDriver[]; onSelect: (factory: string) => void }) { return <section className="rounded-lg border bg-card p-4"><h2 className="mb-3 text-sm font-semibold">Top cost drivers</h2><Table><TableHeader><TableRow><TableHead>Name</TableHead><TableHead className="text-right">Runs</TableHead><TableHead className="text-right">Success</TableHead><TableHead className="text-right">Cost</TableHead><TableHead className="text-right">Cost / merged PR</TableHead><TableHead /></TableRow></TableHeader><TableBody>{drivers.slice(0, 10).map((driver) => <TableRow key={driver.name} className="cursor-pointer" onClick={() => onSelect(driver.name)}><TableCell className="font-medium">{driver.name}</TableCell><TableCell className="text-right tabular-nums">{driver.runs}</TableCell><TableCell className="text-right tabular-nums">{formatPercent(driver.successRate)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costUsd)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costPerMergedPr)}</TableCell><TableCell><ChevronRight className="size-4 text-muted-foreground" /></TableCell></TableRow>)}</TableBody></Table></section> }
function RunsTable({ runs, nextCursor, onSelect, onLoadMore }: { runs: TaskRunSummary[]; nextCursor?: string; onSelect: (runId: string) => void; onLoadMore: () => void }) { return <section className="rounded-lg border bg-card p-4"><h2 className="mb-3 text-sm font-semibold">Runs</h2><Table><TableHeader><TableRow>{["Status", "Ticket", "Model", "Factory/Workflow", "Cost", "Duration", "Start date"].map((label) => <TableHead key={label} className={label === "Status" ? "" : "text-right"}>{label}</TableHead>)}</TableRow></TableHeader><TableBody>{runs.map((run) => <TableRow key={run.runId} className="cursor-pointer" onClick={() => onSelect(run.runId)}><TableCell><StatusBadge status={run.status} /></TableCell><TableCell className="text-right">{run.issueId || "—"}</TableCell><TableCell className="text-right">{run.model || "—"}</TableCell><TableCell className="text-right">{run.factoryName || run.workflowName || "—"}</TableCell><TableCell className="text-right tabular-nums">{usd.format(run.estimatedCostUsd || 0)}</TableCell><TableCell className="text-right tabular-nums">{formatDuration(run.finishedAt - run.startedAt)}</TableCell><TableCell className="text-right tabular-nums">{run.startedAt ? new Date(run.startedAt).toLocaleDateString() : "—"}</TableCell></TableRow>)}</TableBody></Table>{nextCursor && <div className="mt-3 text-center"><Button variant="outline" size="sm" onClick={onLoadMore}>Load more</Button></div>}</section> }
function calculateDelta(current?: number | null, prior?: number | null) { return current == null || prior == null || prior === 0 ? undefined : (current - prior) / prior }
function KpiGroup({ title, children }: { title: string; children: ReactNode }) { return <div><p className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">{title}</p><div className="grid grid-cols-2 gap-2 sm:grid-cols-5">{children}</div></div> }
function Kpi({ label, value, change, good, cost, onClick }: { label: string; value?: string | number; change?: number; good?: boolean; cost?: boolean; onClick?: () => void }) { const bad = cost ? (change ?? 0) > 0 : good ? (change ?? 0) < 0 : (change ?? 0) > 0; return <button type="button" disabled={!onClick} onClick={onClick} className="min-w-0 rounded-lg border bg-card p-3 text-left enabled:hover:bg-accent"><p className="min-h-8 text-xs text-muted-foreground">{label}</p><p className="mt-2 text-xl font-semibold tracking-tight">{value ?? "—"}</p>{change != null && <p className={`text-xs font-medium ${bad ? "text-red-600 dark:text-red-400" : "text-emerald-600 dark:text-emerald-400"}`}>{change > 0 ? "+" : ""}{(change * 100).toFixed(1)}% <span className="font-normal text-muted-foreground">vs prior</span></p>}</button> }
function ChartCard({ title, children }: { title: string; children: ReactNode }) { return <section className="rounded-lg border bg-card p-4"><h2 className="text-sm font-semibold">{title}</h2>{children}</section> }
