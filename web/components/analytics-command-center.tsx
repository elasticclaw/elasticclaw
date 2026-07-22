"use client"

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { Info } from "lucide-react"
import {
  Bar,
  BarChart,
  CartesianGrid,
  LabelList,
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

const commandCenterUrlFilterKeys = ["workspace", ...urlFilterKeys, "from", "to"] as const
// Matches the heatmap's custom tooltip (bg-card/border/shadow-md) so every
// explanatory tooltip in this file shares one look, in both themes.
const tooltipContentClassName = "max-w-xs rounded-lg border bg-card text-foreground shadow-md"
const tooltipArrowClassName = "bg-card fill-card"
const chartConfig = {
  clean: { label: "Clean", color: "#0ca30c" },
  humanInTheLoop: { label: "Human in the loop", color: "#2a78d6" },
  warning: { label: "Warning", color: "#fab219" },
  failed: { label: "Failed", color: "#d03b3b" },
  ticketDelivered: { label: "Delivered", color: "#0ca30c" },
  ticketInProgress: { label: "In progress", color: "#64748b" },
  ticketFailed: { label: "Failed", color: "#d03b3b" },
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
  if (!milliseconds || milliseconds < 0) return "—"
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

export function AnalyticsCommandCenter({ workspaceScope }: { workspaceScope?: string } = {}) {
  const router = useRouter()
  const pathname = usePathname()
  const params = useSearchParams()
  const paramsKey = params.toString()
  const filters = useMemo(() => {
    const nextFilters: TaskRunAnalyticsFilters = {
      analyticsEnabled: true,
      requiresPr: true,
      limit: 10,
    }

    for (const filterKey of commandCenterUrlFilterKeys) {
      const value = params.get(filterKey)
      if (value) (nextFilters as Record<string, string>)[filterKey] = value
    }

    if (workspaceScope && !params.get("workspace")) {
      nextFilters.workspace = workspaceScope
    }

    return nextFilters
  }, [params, paramsKey, workspaceScope])
  const [summary, setSummary] = useState<TaskRunAnalyticsSummary>()
  const [costs, setCosts] = useState<CostOverview>()
  const [yearCosts, setYearCosts] = useState<CostOverview>()
  const [effect, setEffect] = useState<AnalyticsEffectiveness>()
  const [stats, setStats] = useState<GeneralStats>()
  const [drivers, setDrivers] = useState<AnalyticsCostDriver[]>([])
  const [runs, setRuns] = useState<TaskRunSummary[]>([])
  const [options, setOptions] = useState<TaskRunFilterOptions>()
  const [nextCursor, setNextCursor] = useState<string>()
  const [cursorStack, setCursorStack] = useState<(string | undefined)[]>([undefined])
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)
  const [details, setDetails] = useState<DetailState | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [error, setError] = useState<string>()
  const loadAbortController = useRef<AbortController | null>(null)
  const loadRequestId = useRef(0)
  // Track the current page cursor in a ref so the polling effect below can read the
  // latest value on every tick without re-arming the interval when cursorStack changes.
  const cursorStackRef = useRef(cursorStack)

  useEffect(() => {
    cursorStackRef.current = cursorStack
  }, [cursorStack])

  // Cache the last-known run object per selectedRunId so a silent poll that
  // drops the selected run off the current page (e.g. a new run pushes it
  // past the page size) doesn't unmount the open drawer out from under the
  // user. The cache is only trusted while selectedRunId hasn't changed.
  const selectedRunCache = useRef<{ runId: string; run: TaskRunSummary } | null>(null)
  const selectedRun = useMemo(() => {
    if (!selectedRunId) return null
    const found = runs.find((run) => run.runId === selectedRunId) ?? null
    if (found) {
      selectedRunCache.current = { runId: selectedRunId, run: found }
      return found
    }
    if (selectedRunCache.current?.runId === selectedRunId) return selectedRunCache.current.run
    return null
  }, [runs, selectedRunId])

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
    async (cursor?: string, loadOptions: { append?: boolean; silent?: boolean } = {}) => {
      const { append = false, silent = false } = loadOptions
      loadAbortController.current?.abort()
      const controller = new AbortController()
      loadAbortController.current = controller
      const requestId = ++loadRequestId.current
      try {
        if (!silent) setError(undefined)
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
        // The year heatmap always shows the trailing year, regardless of the
        // selected period. taskRunAnalyticsCostsUseRunFilters in
        // pkg/hub/task_run_analytics_costs_api.go must be used whenever a
        // run-level filter is active: scope=ledger reads usage_daily through
        // taskRunAnalyticsUsageDimensionsWhere, which intentionally omits
        // those filters.
        const yearFilters = { ...effectiveFilters, from: undefined, to: undefined }
        const hasYearRunLevelFilter = Boolean(
          yearFilters.repo ||
          yearFilters.status ||
          yearFilters.warningType ||
          yearFilters.failureType ||
          yearFilters.humanTouched != null ||
          yearFilters.mergedPrs != null
        )
        const yearCostFilters = hasYearRunLevelFilter
          ? yearFilters
          : { ...yearFilters, analyticsEnabled: undefined, requiresPr: undefined }
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
          fetchAnalyticsCosts(effectiveFilters, 30, "model", undefined, { signal: controller.signal }),
          fetchAnalyticsCosts(
            yearCostFilters,
            366,
            undefined,
            hasYearRunLevelFilter ? undefined : "ledger",
            { signal: controller.signal }
          ),
          fetchAnalyticsEffectiveness(effectiveFilters, { signal: controller.signal }),
          fetchGeneralStats(effectiveFilters, { signal: controller.signal }),
          fetchAnalyticsCostDrivers(effectiveFilters, "workflow", { signal: controller.signal }),
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
        if (!append && !silent) {
          setSelectedRunId(null)
          setDetails(null)
          setDetailError(null)
        }
      } catch (loadError) {
        if (controller.signal.aborted || (loadError instanceof Error && loadError.name === "AbortError")) return
        if (requestId !== loadRequestId.current) return
        if (!silent) {
          setError(loadError instanceof Error ? loadError.message : "Unable to load analytics")
        }
      }
    },
    [filters, options]
  )

  useEffect(() => {
    setCursorStack([undefined])
    queueMicrotask(() => void load())
    return () => {
      loadAbortController.current?.abort()
    }
  }, [load])

  const silentRefresh = useCallback(() => {
    const currentCursor = cursorStackRef.current[cursorStackRef.current.length - 1]
    void load(currentCursor, { append: false, silent: true })
  }, [load])

  useEffect(() => {
    const tick = () => {
      if (document.hidden) return
      silentRefresh()
    }
    const intervalId = setInterval(tick, 60_000)

    const handleVisibilityChange = () => {
      if (!document.hidden) {
        // Tab just became visible again — refresh immediately rather than waiting for the next tick.
        silentRefresh()
      }
    }
    document.addEventListener("visibilitychange", handleVisibilityChange)

    return () => {
      clearInterval(intervalId)
      document.removeEventListener("visibilitychange", handleVisibilityChange)
    }
    // Re-arm when load changes for new filters/options; each tick reads the latest cursor from the ref.
  }, [silentRefresh])

  const handleNextPage = useCallback(() => {
    if (!nextCursor) return
    setCursorStack((currentStack) => [...currentStack, nextCursor])
    void load(nextCursor)
  }, [load, nextCursor])

  const handlePreviousPage = useCallback(() => {
    if (cursorStack.length <= 1) return
    const nextStack = cursorStack.slice(0, -1)
    setCursorStack(nextStack)
    void load(nextStack[nextStack.length - 1])
  }, [cursorStack, load])

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
  const selectedDay = filters.from && filters.to && (() => {
    const day = filters.from.slice(0, 10)
    const range = isoDayRange(day)
    return range.from === filters.from && range.to === filters.to ? day : undefined
  })()
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
          <KpiGroup title="Effectiveness" columns="sm:grid-cols-6">
            <Kpi
              label="Runs"
              title="Task runs started in the selected period."
              value={summary?.totalRuns}
              good
              change={calculateDelta(summary?.totalRuns, summary?.prior?.totalRuns)}
              onClick={() => setFilters({ status: undefined })}
            />
            <Kpi
              label="Unique tickets"
              title="Distinct tickets worked on in the selected period. A ticket may have several runs."
              value={effect?.uniqueTickets}
              good
              change={calculateDelta(effect?.uniqueTickets, effect?.prior?.uniqueTickets)}
            />
            <Kpi
              label="Success rate (runs)"
              title="Of the runs that finished, the share that delivered their work (pull request merged or closed) — with or without human help."
              value={formatPercent(effect?.successRate)}
              good
              change={calculateDelta(effect?.successRate, effect?.prior?.successRate)}
              onClick={() => setFilters({ status: undefined })}
            />
            <Kpi
              label="Success rate (unique tickets)"
              title="Of the distinct tickets with at least one finished run, the share where at least one run delivered (clean, human in the loop, or warning)."
              value={formatPercent(effect?.ticketSuccessRate)}
              good
              change={calculateDelta(effect?.ticketSuccessRate, effect?.prior?.ticketSuccessRate)}
            />
            <Kpi
              label="Avg ticket to PR"
              title="Average time from the ticket being created to the PR being opened (ready for review)."
              value={formatDuration(stats?.ticketToPrMs.avgMs)}
              change={calculateDelta(stats?.ticketToPrMs.avgMs, stats?.prior?.ticketToPrMs.avgMs)}
            />
            <Kpi
              label="Avg PR to merge"
              title="Average time from the PR being opened to it being merged."
              value={formatDuration(stats?.prOpenToMergeMs.avgMs)}
              change={calculateDelta(stats?.prOpenToMergeMs.avgMs, stats?.prior?.prOpenToMergeMs.avgMs)}
            />
          </KpiGroup>
          <KpiGroup title="Cost" columns="sm:grid-cols-3">
            <Kpi label="Total cost" title="Total AI spend of the runs in the selected period." value={usdWhole.format(totalCost)} change={costDelta} cost />
            <Kpi
              label="Cost per run"
              title="Total cost divided by the number of runs."
              value={usd.format(summary?.totalRuns ? totalCost / summary.totalRuns : 0)}
              cost
            />
            <Kpi
              label="Cost per unique ticket"
              title="Total cost divided by the number of distinct tickets worked on in the period."
              value={effect?.uniqueTickets ? usd.format(totalCost / effect.uniqueTickets) : "—"}
              change={calculateDelta(
                effect?.uniqueTickets ? totalCost / effect.uniqueTickets : undefined,
                priorCost && effect?.prior?.uniqueTickets ? priorCost / effect.prior.uniqueTickets : undefined
              )}
              cost
            />
          </KpiGroup>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Run outcomes over time" info="Each bar is a day. Clean = delivered with no human help; Human in the loop = a human helped via the pull request; Warning = a human had to step in from the dashboard; Failed = nothing was delivered.">
            <OutcomesChart effect={effect} />
          </ChartCard>
          <ChartCard title="Delivery funnel" info="How many runs made it from the agent starting, to opening a pull request, to that pull request being finished (merged or closed). Percentages show the conversion from the previous stage.">
            <DeliveryFunnel effect={effect} />
          </ChartCard>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Ticket throughput" info="Each bar is a day: how many distinct tickets had their first run that day, by how the ticket ended up. Delivered = at least one run delivered the work.">
            <TicketThroughputChart effect={effect} />
          </ChartCard>
          <ChartCard title="Runs per ticket" info="How many runs each ticket needed. A long tail of 3+ means lots of retries on the same tickets.">
            <RunsPerTicketChart effect={effect} />
          </ChartCard>
        </div>

        <RunsTable
          runs={runs}
          page={cursorStack.length}
          canGoPrevious={cursorStack.length > 1}
          canGoNext={Boolean(nextCursor)}
          onSelect={setSelectedRunId}
          onPrevious={handlePreviousPage}
          onNext={handleNextPage}
        />

        <Heatmap
          heatmap={heatmap}
          maxCost={maxHeatCost}
          selectedDay={selectedDay}
          onSelectDay={(day) => setFilters(day === selectedDay ? { from: undefined, to: undefined } : isoDayRange(day))}
          onClearSelectedDay={() => setFilters({ from: undefined, to: undefined })}
        />

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Daily cost by model" info="How much was spent per day, split by AI model.">
            <DailyCostChart costs={costs} modelData={modelData} />
          </ChartCard>
          <ChartCard title="Cost per merged PR" info="Weekly average of what one merged pull request cost. The reference line is the period average.">
            <CostPerMergedPrChart effect={effect} />
          </ChartCard>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Workflow cost comparison" info="Daily spend of the most expensive workflows in the selected period, compared side by side.">
            <WorkflowCostComparisonChart drivers={drivers} />
          </ChartCard>
          <ChartCard title="Most expensive tickets" info="Where the money concentrates: the costliest tickets in the selected period (cost counts only this period's runs).">
            <TopTicketsByCostChart effect={effect} />
          </ChartCard>
        </div>
        <CostDrivers drivers={drivers} />
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

function Heatmap({ heatmap, maxCost, selectedDay, onSelectDay, onClearSelectedDay }: { heatmap: ReturnType<typeof useHeatmap>; maxCost: number; selectedDay?: string; onSelectDay: (day: string) => void; onClearSelectedDay: () => void }) {
  const containerRef = useRef<HTMLElement>(null)
  const [tooltip, setTooltip] = useState<{ iso: string; point: (typeof heatmap.days)[number]["point"]; x: number; y: number } | null>(null)
  const showTooltip = (iso: string, point: (typeof heatmap.days)[number]["point"], clientX: number, clientY: number) => {
    const rect = containerRef.current?.getBoundingClientRect()
    if (rect) setTooltip({ iso, point, x: clientX - rect.left + 12, y: clientY - rect.top + 12 })
  }
  const selectedDayLabel = selectedDay && new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" }).format(new Date(`${selectedDay}T00:00:00`))

  return <section ref={containerRef} className="relative rounded-lg border bg-card p-4"><div className="mb-4 flex items-center gap-1"><h2 className="text-sm font-semibold">Cost by day</h2><InfoTooltip text="Each square is a day; darker means more spend. Click a day to focus the whole page on it." />{selectedDayLabel && <span className="ml-2 inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">{selectedDayLabel}<button type="button" aria-label={`Clear ${selectedDayLabel} filter`} onClick={onClearSelectedDay} className="rounded-full px-0.5 text-foreground hover:bg-accent">×</button></span>}</div><div className="grid grid-cols-[24px_1fr] gap-2"><div className="pt-5 text-center text-[10px] text-muted-foreground"><div className="grid grid-rows-7">{["M", "T", "W", "T", "F", "S", "S"].map((day, index) => <span key={`${day}-${index}`}>{day}</span>)}</div></div><div><div className="relative mb-1 h-4 text-[10px] text-muted-foreground">{heatmap.monthLabels.map(({ week, label }) => <span key={`${week}-${label}`} className="absolute" style={{ left: `${(week / 52) * 100}%` }}>{label}</span>)}</div><div className="grid grid-flow-col grid-cols-[repeat(52,minmax(0,1fr))] grid-rows-7 gap-1">{heatmap.days.map(({ iso, point }) => { const level = point?.costUsd ? Math.min(5, Math.ceil((point.costUsd / maxCost) * 5)) : 0; const selected = iso === selectedDay; return <button key={iso} onClick={() => onSelectDay(iso)} onMouseEnter={(event) => showTooltip(iso, point, event.clientX, event.clientY)} onMouseMove={(event) => showTooltip(iso, point, event.clientX, event.clientY)} onMouseLeave={() => setTooltip(null)} onFocus={(event) => { const rect = event.currentTarget.getBoundingClientRect(); showTooltip(iso, point, rect.left + rect.width / 2, rect.top + rect.height / 2) }} onBlur={() => setTooltip(null)} className={`aspect-square cursor-pointer rounded-sm border border-black/5 transition-shadow hover:ring-2 hover:ring-ring hover:ring-offset-1 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 ${selected ? "ring-2 ring-primary ring-offset-1" : ""}`} style={{ background: level ? `var(--heatmap-${level})` : "var(--muted)" }} /> })}</div></div></div>{tooltip && <div role="tooltip" className="pointer-events-none absolute z-10 rounded-lg border bg-card px-2 py-1.5 text-xs text-foreground shadow-md" style={{ left: tooltip.x, top: tooltip.y }}><div>{new Intl.DateTimeFormat(undefined, { weekday: "short", month: "short", day: "numeric", year: "numeric" }).format(new Date(`${tooltip.iso}T00:00:00`))}</div><div className="text-muted-foreground">{usdWhole.format(tooltip.point?.costUsd ?? 0)} · {tooltip.point?.runCount ?? 0} runs</div></div>}<div className="mt-3 flex justify-end gap-1 text-xs text-muted-foreground">Less {Array.from({ length: 5 }, (_, index) => <i key={index} className="size-3 rounded-sm" style={{ background: `var(--heatmap-${index + 1})` }} />)} More</div></section>
}

function useModelData(costs?: CostOverview) { return useMemo(() => { const models = (costs?.seriesByModel ?? []).slice(0, 4); return (costs?.dailySeries ?? []).map((day, index) => ({ date: day.date, Other: Math.max(0, day.costUsd - models.reduce((sum, model) => sum + (model.dailySeries[index]?.costUsd ?? 0), 0)), ...Object.fromEntries(models.map((model) => [model.model, model.dailySeries[index]?.costUsd ?? 0])) })) }, [costs]) }
function useWorkflowCostComparisonData(drivers: AnalyticsCostDriver[]) { return useMemo(() => { const topDrivers = [...drivers].sort((a, b) => b.costUsd - a.costUsd).slice(0, 5); const topNames = topDrivers.map((driver) => driver.name); const topNameSet = new Set(topNames); const dataByDate = new Map<string, Record<string, string | number>>(); for (const driver of drivers) for (const point of driver.dailyCost) { const row = dataByDate.get(point.date) ?? { date: point.date, Other: 0, ...Object.fromEntries(topNames.map((name) => [name, 0])) }; if (topNameSet.has(driver.name)) row[driver.name] = Number(row[driver.name]) + point.costUsd; else row.Other = Number(row.Other) + point.costUsd; dataByDate.set(point.date, row) } return { data: [...dataByDate.values()].sort((a, b) => String(a.date).localeCompare(String(b.date))), topNames } }, [drivers]) }
function DailyCostChart({ costs, modelData }: { costs?: CostOverview; modelData: Record<string, string | number>[] }) { const labels = [...(costs?.seriesByModel ?? []).slice(0, 4).map((item) => item.model), "Other"]; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={modelData}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><Legend />{labels.map((label, index) => <Bar key={label} dataKey={label} name={label} stackId="cost" fill={`var(--chart-${index + 1})`} />)}</BarChart></ChartContainer> }
function WorkflowCostComparisonChart({ drivers }: { drivers: AnalyticsCostDriver[] }) { const { data, topNames } = useWorkflowCostComparisonData(drivers); if (drivers.length === 0) return <p className="py-8 text-center text-sm text-muted-foreground">No cost data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><LineChart data={data}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><Legend />{topNames.map((name, index) => <Line key={name} type="monotone" dataKey={name} name={name} stroke={`var(--chart-${index + 1})`} dot={false} strokeWidth={2} />)}{drivers.length > 5 && <Line type="monotone" dataKey="Other" name="Other" stroke="var(--muted-foreground)" dot={false} strokeWidth={2} />}</LineChart></ChartContainer> }
function OutcomesChart({ effect }: { effect?: AnalyticsEffectiveness }) { return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect?.outcomesByDay}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis allowDecimals={false} /><ChartTooltip content={<ChartTooltipContent />} /><Legend /><Bar dataKey="clean" name="Clean" stackId="outcome" fill="var(--color-clean)" /><Bar dataKey="humanInTheLoop" name="Human in the loop" stackId="outcome" fill="var(--color-humanInTheLoop)" /><Bar dataKey="warning" name="Warning" stackId="outcome" fill="var(--color-warning)" /><Bar dataKey="failed" name="Failed" stackId="outcome" fill="var(--color-failed)" /></BarChart></ChartContainer> }
function DeliveryFunnel({ effect }: { effect?: AnalyticsEffectiveness }) {
  const stages = [
    ["agentStarted", "Agent started"],
    ["prOpened", "PR opened"],
    ["prFinished", "PR finished"],
  ] as const

  return <div className="space-y-3 pt-3">{stages.map(([name, label], index) => {
    const value = effect?.funnel[name] ?? 0
    const previous = index ? effect?.funnel[stages[index - 1][0]] ?? 0 : 0
    return <div key={name}><div className="mb-1 flex justify-between text-sm"><span>{label}</span><span className="tabular-nums">{value} {index ? `(${formatPercent(previous ? value / previous : undefined)})` : ""}</span></div><div className="h-5 rounded bg-muted"><div className="h-full rounded bg-chart-1" style={{ width: `${(value / (effect?.funnel.agentStarted || 1)) * 100}%` }} /></div></div>
  })}</div>
}
function TicketThroughputChart({ effect }: { effect?: AnalyticsEffectiveness }) { if (!effect?.ticketsByDay?.length) return <p className="py-8 text-center text-sm text-muted-foreground">No ticket data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect.ticketsByDay}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis allowDecimals={false} /><ChartTooltip content={<ChartTooltipContent />} /><Legend /><Bar dataKey="delivered" name="Delivered" stackId="ticket" fill="var(--color-ticketDelivered)" /><Bar dataKey="inProgress" name="In progress" stackId="ticket" fill="var(--color-ticketInProgress)" /><Bar dataKey="failed" name="Failed" stackId="ticket" fill="var(--color-ticketFailed)" /></BarChart></ChartContainer> }
function RunsPerTicketChart({ effect }: { effect?: AnalyticsEffectiveness }) { if (!effect?.runsPerTicket?.length) return <p className="py-8 text-center text-sm text-muted-foreground">No ticket data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect.runsPerTicket} layout="vertical" margin={{ right: 36 }}><CartesianGrid horizontal={false} /><XAxis type="number" allowDecimals={false} /><YAxis type="category" dataKey="bucket" width={40} /><Bar dataKey="tickets" fill="var(--chart-1)" radius={4}><LabelList dataKey="tickets" position="right" /></Bar></BarChart></ChartContainer> }
function TopTicketTooltip({ active, payload }: { active?: boolean; payload?: { payload?: AnalyticsEffectiveness["topTicketsByCost"][number] }[] }) { const ticket = payload?.[0]?.payload; if (!active || !ticket) return null; const outcome = ticket.outcome === "in_progress" ? "In progress" : ticket.outcome ? ticket.outcome.charAt(0).toUpperCase() + ticket.outcome.slice(1) : "—"; return <div className={`${tooltipContentClassName} px-3 py-2 text-xs`}><div className="font-medium">{ticket.issueTitle}</div><div className="text-muted-foreground">{usd.format(ticket.costUsd)} · {ticket.runs} runs · {outcome}</div></div> }
function TopTicketsByCostChart({ effect }: { effect?: AnalyticsEffectiveness }) { if (!effect?.topTicketsByCost?.length) return <p className="py-8 text-center text-sm text-muted-foreground">No ticket data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect.topTicketsByCost} layout="vertical" margin={{ right: 36 }}><CartesianGrid horizontal={false} /><XAxis type="number" tickFormatter={(value) => usd.format(value)} /><YAxis type="category" dataKey="issueId" width={90} interval={0} tickFormatter={(id) => (id.length > 14 ? id.slice(0, 13) + "…" : id)} /><ChartTooltip content={<TopTicketTooltip />} /><Bar dataKey="costUsd" fill="var(--chart-1)" radius={4}><LabelList dataKey="costUsd" position="right" formatter={(value: number) => usd.format(value)} /></Bar></BarChart></ChartContainer> }
function CostPerMergedPrChart({ effect }: { effect?: AnalyticsEffectiveness }) { return <ChartContainer config={chartConfig} className="h-64 w-full"><LineChart data={effect?.costPerMergedPr.weekly}><CartesianGrid vertical={false} /><XAxis dataKey="weekStart" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><ReferenceLine y={effect?.costPerMergedPr.average} stroke="var(--muted-foreground)" /><Line type="monotone" dataKey="costPerMergedPr" name="Cost per merged PR" stroke="var(--color-costPerMergedPr)" dot={false} /></LineChart></ChartContainer> }
function CostDrivers({ drivers }: { drivers: AnalyticsCostDriver[] }) {
  return <section className="rounded-lg border bg-card p-4"><div className="mb-3 flex items-center justify-between gap-3"><div><div className="flex items-center gap-1"><h2 className="text-sm font-semibold">Top cost drivers</h2><InfoTooltip text="Where the money goes: total spend and efficiency per workflow in the selected period." /></div><p className="text-xs text-muted-foreground">By workflow</p></div></div><Table><TableHeader><TableRow><TableHead>Workflow</TableHead><TableHead className="text-right">Runs</TableHead><TableHead className="text-right">Success</TableHead><TableHead className="text-right">Cost</TableHead><TableHead className="text-right">Cost / merged PR</TableHead></TableRow></TableHeader><TableBody>{drivers.slice(0, 10).map((driver) => <TableRow key={driver.name}><TableCell className="font-medium">{driver.name}</TableCell><TableCell className="text-right tabular-nums">{driver.runs}</TableCell><TableCell className="text-right tabular-nums">{formatPercent(driver.successRate)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costUsd)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costPerMergedPr)}</TableCell></TableRow>)}</TableBody></Table></section>
}
function RunsTable({ runs, page, canGoPrevious, canGoNext, onSelect, onPrevious, onNext }: { runs: TaskRunSummary[]; page: number; canGoPrevious: boolean; canGoNext: boolean; onSelect: (runId: string) => void; onPrevious: () => void; onNext: () => void }) { return <section className="rounded-lg border bg-card p-4"><h2 className="mb-3 text-sm font-semibold">Runs</h2><Table><TableHeader><TableRow>{["Status", "Ticket", "Model", "Factory/Workflow", "Cost", "Duration", "Start date"].map((label) => <TableHead key={label} className={label === "Status" ? "" : "text-right"}>{label}</TableHead>)}</TableRow></TableHeader><TableBody>{runs.map((run) => <TableRow key={run.runId} className="cursor-pointer" onClick={() => onSelect(run.runId)}><TableCell><StatusBadge status={run.status} /></TableCell><TableCell className="text-right">{run.issueId || "—"}</TableCell><TableCell className="text-right">{run.model || "—"}</TableCell><TableCell className="text-right">{run.factoryName || run.workflowName || "—"}</TableCell><TableCell className="text-right tabular-nums">{usd.format(run.estimatedCostUsd || 0)}</TableCell><TableCell className="text-right tabular-nums">{formatDuration(run.finishedAt ? run.finishedAt - run.startedAt : undefined)}</TableCell><TableCell className="text-right tabular-nums">{run.startedAt ? new Date(run.startedAt).toLocaleDateString() : "—"}</TableCell></TableRow>)}</TableBody></Table><div className="mt-3 flex items-center justify-center gap-3"><Button variant="outline" size="sm" disabled={!canGoPrevious} onClick={onPrevious}>Previous</Button><span className="text-sm text-muted-foreground">Page {page}</span><Button variant="outline" size="sm" disabled={!canGoNext} onClick={onNext}>Next</Button></div></section> }
function calculateDelta(current?: number | null, prior?: number | null) { return current == null || prior == null || prior === 0 ? undefined : (current - prior) / prior }
function KpiGroup({ title, columns = "sm:grid-cols-5", children }: { title: string; columns?: string; children: ReactNode }) { return <div><p className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">{title}</p><div className={`grid grid-cols-2 gap-2 ${columns}`}>{children}</div></div> }
function Kpi({ label, value, change, good, cost, onClick, title }: { label: string; value?: string | number; change?: number; good?: boolean; cost?: boolean; onClick?: () => void; title?: string }) {
  const bad = cost ? (change ?? 0) > 0 : good ? (change ?? 0) < 0 : (change ?? 0) > 0
  const disabled = !onClick
  const button = (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className="flex h-full w-full min-w-0 flex-col rounded-lg border bg-card p-3 text-left"
    >
      <p className="flex min-h-8 items-center gap-1 text-xs text-muted-foreground">
        {label}
        {title && <Info aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />}
      </p>
      <p className="mt-2 text-xl font-semibold tracking-tight">{value ?? "—"}</p>
      {/* Always render the delta row, even when there's no change, so every
          tile reserves the same space and labels/values align across the grid. */}
      <p
        className={`truncate text-xs font-medium ${
          change == null ? "invisible" : bad ? "text-red-600 dark:text-red-400" : "text-emerald-600 dark:text-emerald-400"
        }`}
      >
        {change != null ? (
          <>
            {change > 0 ? "+" : ""}
            {(change * 100).toFixed(1)}% <span className="font-normal text-muted-foreground">vs prior</span>
          </>
        ) : (
          " "
        )}
      </p>
    </button>
  )
  if (!title) return button
  return (
    <Tooltip delayDuration={200}>
      <TooltipTrigger asChild>
        {disabled ? (
          <span className="block h-full w-full" tabIndex={0}>
            {button}
          </span>
        ) : (
          button
        )}
      </TooltipTrigger>
      <TooltipContent className={tooltipContentClassName} arrowClassName={tooltipArrowClassName}>
        {title}
      </TooltipContent>
    </Tooltip>
  )
}
function InfoTooltip({ text }: { text: string }) {
  return (
    <Tooltip delayDuration={200}>
      <TooltipTrigger asChild>
        <button type="button" className="cursor-help text-muted-foreground">
          <Info className="size-3.5" />
        </button>
      </TooltipTrigger>
      <TooltipContent className={tooltipContentClassName} arrowClassName={tooltipArrowClassName}>
        {text}
      </TooltipContent>
    </Tooltip>
  )
}
function ChartCard({ title, info, children }: { title: string; info?: string; children: ReactNode }) { return <section className="rounded-lg border bg-card p-4"><div className="flex items-center gap-1"><h2 className="text-sm font-semibold">{title}</h2>{info && <InfoTooltip text={info} />}</div>{children}</section> }
