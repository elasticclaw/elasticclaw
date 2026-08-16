"use client"

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { usePathname, useSearchParams } from "next/navigation"
import { Info } from "lucide-react"
import {
  Bar,
  BarChart,
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
  fetchWorkspaces,
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
  formatLabel,
  type DetailState,
  urlFilterKeys,
} from "@/components/task-run-analytics-view"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
} from "@/components/ui/select"
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
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
const tooltipContentClassName = "max-w-xs rounded-lg border border-border bg-card text-card-foreground shadow-ds-md"
const tooltipArrowClassName = "bg-card fill-card"
// One semantic scale for every chart on this screen. Outcome series always read
// as outcome colors (clean = ok, human = neutral, warning = warn, failed =
// accent); money and volume series read as the single data-blue ramp, so cost
// can never be mistaken for an outcome. Everything points at a theme token so
// the charts re-tint with the design system rather than at a frozen hex.
const chartConfig = {
  clean: { label: "Clean", color: "var(--color-status-ok)" },
  humanInTheLoop: { label: "Human on the loop", color: "var(--ds-neutral-500)" },
  warning: { label: "Warning", color: "var(--color-status-warn)" },
  failed: { label: "Failed", color: "var(--color-primary)" },
  ticketDelivered: { label: "Delivered", color: "var(--color-status-ok)" },
  ticketInProgress: { label: "In progress", color: "var(--ds-neutral-500)" },
  ticketFailed: { label: "Failed", color: "var(--color-primary)" },
  costPerMergedPr: { label: "Cost / merged PR", color: "var(--color-data)" },
} satisfies ChartConfig
// — plot language —
// The mockup's charts are almost bare: no cartesian grid, no axis lines, no
// tick marks. Only two hairlines (the mid rule and the baseline) hold the
// vertical scale, and each axis is reduced to three mono labels. Everything
// below expresses that in Recharts terms so every plot on the page reads the
// same way.
const hairline = { stroke: "var(--color-foreground)", strokeOpacity: 0.1, strokeWidth: 1 } as const
const axisProps = { axisLine: false, tickLine: false, tickMargin: 8, interval: 0 } as const
const plotMargin = { top: 6, right: 6, left: 0, bottom: 0 } as const

// Round a max up to a value whose half is still a readable number, so the mid
// hairline always lands on the middle label.
function niceCeiling(value: number) {
  if (!Number.isFinite(value) || value <= 0) return 1
  const step = 10 ** Math.floor(Math.log10(value))
  const normalized = value / step
  const rounded = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 4 ? 4 : normalized <= 5 ? 5 : 10
  return rounded * step
}

function yScale(maxValue: number) {
  const max = niceCeiling(maxValue)
  return { domain: [0, max] as [number, number], ticks: [0, max / 2, max], mid: max / 2 }
}

// Three x labels: the start, the middle and the end of the range.
function edgeTicks<Row>(rows: Row[] | undefined, pick: (row: Row) => string) {
  if (!rows?.length) return undefined
  if (rows.length < 3) return rows.map(pick)
  return [rows[0], rows[Math.floor((rows.length - 1) / 2)], rows[rows.length - 1]].map(pick)
}

function stackedMax(rows: readonly Record<string, unknown>[] | undefined, keys: readonly string[]) {
  return Math.max(
    0,
    ...(rows ?? []).map((row) => keys.reduce((sum, key) => sum + (Number(row[key]) || 0), 0))
  )
}

function seriesMax(rows: readonly Record<string, unknown>[] | undefined, keys: readonly string[]) {
  return Math.max(0, ...(rows ?? []).flatMap((row) => keys.map((key) => Number(row[key]) || 0)))
}
// Money and volume series step down the single data-blue ramp and end on the
// neutrals, so a cost series is never confused with an outcome color.
const costSeriesColors = [
  "var(--color-data)",
  "var(--color-data-light)",
  "var(--color-data-dark)",
  "var(--ds-neutral-600)",
  "var(--ds-neutral-400)",
]
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

const periodOptions = [
  ["7d", 7],
  ["30d", 30],
  ["90d", 90],
  ["MTD", 0],
] as const

const monthDay = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" })
const monthDayYear = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" })

// Caption under the page title: the range the whole screen is currently scoped
// to. Derived from the range the loader actually applied, never from the clock.
function formatPeriodRange(from?: string, to?: string) {
  if (!from || !to) return undefined
  return `${monthDay.format(new Date(from))} – ${monthDayYear.format(new Date(to))}`
}

// Which segment of the period control the applied range corresponds to, if any
// (a heatmap day selection matches none of them).
function activePeriodLabel(from?: string, to?: string) {
  if (!from || !to) return undefined
  const start = new Date(from)
  const end = new Date(to)
  const days = Math.round((end.getTime() - start.getTime()) / 86_400_000)
  const exact = periodOptions.find(([, span]) => span > 0 && span === days)
  if (exact) return exact[0]
  const sameMonth = start.getDate() === 1 && start.getMonth() === end.getMonth() && start.getFullYear() === end.getFullYear()
  return sameMonth ? "MTD" : undefined
}

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

export function AnalyticsCommandCenter() {
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

    return nextFilters
  }, [params])
  const [workspaceNames, setWorkspaceNames] = useState<string[]>([])

  useEffect(() => {
    let cancelled = false
    fetchWorkspaces()
      .then((data) => { if (!cancelled) setWorkspaceNames(data.map((workspace) => workspace.name)) })
      .catch(() => { if (!cancelled) setWorkspaceNames([]) })
    return () => { cancelled = true }
  }, [])
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
  // The range the loader actually used — the URL may carry none, in which case
  // it defaults to the last 30 days. Kept in state so the header can caption
  // the period without reading the clock during render.
  const [appliedRange, setAppliedRange] = useState<{ from?: string; to?: string }>({})
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
  // Held in state and adjusted during render (React's "adjusting state when
  // props change" pattern) so no ref is read or written while rendering.
  const [selectedRunCache, setSelectedRunCache] = useState<{ runId: string; run: TaskRunSummary } | null>(null)
  const foundRun = selectedRunId ? runs.find((run) => run.runId === selectedRunId) ?? null : null
  if (selectedRunId && foundRun && (selectedRunCache?.runId !== selectedRunId || selectedRunCache.run !== foundRun)) {
    setSelectedRunCache({ runId: selectedRunId, run: foundRun })
  }
  const selectedRun = selectedRunId
    ? foundRun ?? (selectedRunCache?.runId === selectedRunId ? selectedRunCache.run : null)
    : null

  const setFilters = useCallback(
    (updates: Record<string, string | undefined>) => {
      const nextParams = new URLSearchParams(paramsKey)
      Object.entries(updates).forEach(([filterKey, value]) => {
        if (value) nextParams.set(filterKey, value)
        else nextParams.delete(filterKey)
      })
      nextParams.delete("cursor")
      // Native history API instead of router.replace: Next syncs
      // usePathname/useSearchParams with replaceState, and this avoids a
      // client navigation that would remount the shared home shell (killing
      // the hub WebSocket) when analytics renders as a view of "/".
      window.history.replaceState(null, "", `${pathname}${nextParams.size ? `?${nextParams}` : ""}`)
    },
    [paramsKey, pathname]
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
        setAppliedRange({ from: effectiveFilters.from, to: effectiveFilters.to })
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
    queueMicrotask(() => {
      setCursorStack([undefined])
      void load()
    })
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
  const periodCaption = formatPeriodRange(appliedRange.from, appliedRange.to)
  const activePeriod = activePeriodLabel(appliedRange.from, appliedRange.to)

  return (
    <main className="h-full overflow-auto bg-background">
      {/* Sticky head: the title, the period it is scoped to, and the filters
          that scope everything below — they stay reachable while scrolling. */}
      <header className="sticky top-0 z-10 border-b-2 border-border bg-background px-5 pt-4 pb-3 lg:px-6">
        <div className="mx-auto grid max-w-[1400px] gap-3">
          <div className="flex flex-wrap items-baseline gap-3">
            <h1 className="text-2xl tracking-tight">Analytics</h1>
            {periodCaption && <span className="text-[13px] text-muted-foreground">{periodCaption}</span>}
            <div className="ml-auto">
              <PeriodControl active={activePeriod} onChange={setFilters} />
            </div>
          </div>
          <FilterBar filters={filters} options={options} workspaces={workspaceNames} onChange={setFilters} />
        </div>
      </header>
      <div className="mx-auto max-w-[1400px] space-y-5 p-5 lg:p-6">
        {error && <p className="text-sm text-destructive">{error}</p>}

        <div className="grid gap-5 xl:grid-cols-[6fr_3fr]">
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
              title="Of the distinct tickets with at least one finished run, the share where at least one run delivered (clean, human on the loop, or warning)."
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
              label="Avg ready→merge"
              title="Average business-hours time (your timezone) from ready-for-review to merge."
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
          <ChartCard
            title="Run outcomes over time"
            description="Each bar is a day. Clean = delivered with no human help."
            info="Each bar is a day. Clean = delivered with no human help; Human on the loop = a human helped via the pull request; Warning = a human had to step in from the dashboard; Failed = nothing was delivered."
          >
            <OutcomesChart effect={effect} />
          </ChartCard>
          <ChartCard
            title="Delivery funnel"
            description="From the agent starting to the pull request being finished."
            info="How many runs made it from the agent starting, to opening a pull request, to that pull request being finished (merged or closed). Percentages show the conversion from the previous stage."
          >
            <DeliveryFunnel effect={effect} />
          </ChartCard>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard
            title="Ticket throughput"
            description="Distinct tickets by the day of their first run, by outcome."
            info="Each bar is a day: how many distinct tickets had their first run that day, by how the ticket ended up. Delivered = at least one run delivered the work."
          >
            <TicketThroughputChart effect={effect} />
          </ChartCard>
          <ChartCard
            title="Runs per ticket"
            description="A long tail of 3+ means repeated retries on the same tickets."
          >
            <RunsPerTicketChart effect={effect} />
          </ChartCard>
        </div>

        <Heatmap
          heatmap={heatmap}
          maxCost={maxHeatCost}
          selectedDay={selectedDay}
          onSelectDay={(day) => setFilters(day === selectedDay ? { from: undefined, to: undefined } : isoDayRange(day))}
          onClearSelectedDay={() => setFilters({ from: undefined, to: undefined })}
        />

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Daily cost by model" description="Spend per day, split by AI model.">
            <DailyCostChart costs={costs} modelData={modelData} />
          </ChartCard>
          <ChartCard
            title="Cost per merged PR"
            description="Weekly average; the dashed line is the period average."
            info="Weekly average of what one merged pull request cost. The reference line is the period average."
          >
            <CostPerMergedPrChart effect={effect} />
          </ChartCard>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Workflow cost comparison" description="Daily spend of the most expensive workflows, side by side.">
            <WorkflowCostComparisonChart drivers={drivers} />
          </ChartCard>
          <ChartCard
            title="Most expensive tickets"
            description="Where the money concentrates in the selected period."
            info="Cost counts only the runs inside the selected period."
          >
            <TopTicketsByCostChart effect={effect} />
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

const allWorkspacesValue = "__all"

function WorkspaceSelect({
  value,
  workspaces,
  onChange,
}: {
  value?: string
  workspaces: string[]
  onChange: (value?: string) => void
}) {
  // Keep a workspace coming from a shared URL selectable even if it isn't in
  // (or hasn't loaded into) the workspace list yet.
  const values = value && !workspaces.includes(value) ? [value, ...workspaces] : workspaces

  return (
    <Select
      value={value ?? allWorkspacesValue}
      onValueChange={(next) => onChange(next === allWorkspacesValue ? undefined : next)}
    >
      <SelectTrigger size="sm" className="w-full text-[13px] font-medium">
        <span className="truncate">
          <span className="text-muted-foreground font-normal">Workspace:</span> {value ?? "All workspaces"}
        </span>
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={allWorkspacesValue}>All workspaces</SelectItem>
        {values.map((workspace) => (
          <SelectItem key={workspace} value={workspace}>{workspace}</SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function FilterBar({
  filters,
  options,
  workspaces,
  onChange,
}: {
  filters: TaskRunAnalyticsFilters
  options?: TaskRunFilterOptions
  workspaces: string[]
  onChange: (updates: Record<string, string | undefined>) => void
}) {
  const selectFilters = [
    ["Factory", "factory", options?.factories],
    ["Workflow", "workflow", options?.workflows],
    ["Repo", "repo", options?.repos],
    ["Model", "model", options?.models],
  ] as const
  // Chips restate what is currently scoping the page — the same values the
  // selects hold, nothing more — and their × clears that one filter.
  const applied = [
    ["Workspace", "workspace", filters.workspace] as const,
    ...selectFilters.map(([label, key]) => [label, key, filters[key]] as const),
  ].filter(([, , value]) => Boolean(value))

  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="w-[190px]">
        <WorkspaceSelect
          value={filters.workspace}
          workspaces={workspaces}
          onChange={(value) => onChange({ workspace: value })}
        />
      </div>
      {/* Hairline that separates the workspace scope from the run filters. */}
      <span aria-hidden className="mx-1 h-6 w-px bg-border" />
      {selectFilters.map(([label, key, values]) => (
        <div key={key} className="w-[168px]">
          <FilterSelect
            label={label}
            value={filters[key]}
            values={values}
            onChange={(value) => onChange({ [key]: value })}
          />
        </div>
      ))}
      {applied.map(([label, key, value]) => (
        <span
          key={key}
          className="inline-flex items-center gap-1.5 rounded-full bg-tint-accent px-2.5 py-[3px] text-xs text-primary"
        >
          {label}: {formatLabel(String(value))}
          <button
            type="button"
            aria-label={`Clear the ${label.toLowerCase()} filter`}
            onClick={() => onChange({ [key]: undefined })}
            className="-mr-1 rounded-full px-1 leading-none hover:bg-primary/15"
          >
            ×
          </button>
        </span>
      ))}
      {applied.length > 0 && (
        <Button
          variant="ghost"
          size="sm"
          className="h-8 px-2 text-xs text-primary hover:bg-primary/10 hover:text-primary"
          onClick={() => onChange(Object.fromEntries(applied.map(([, key]) => [key, undefined])))}
        >
          Clear filters
        </Button>
      )}
    </div>
  )
}

// The period segmented control: one active segment on the accent, the rest
// plain, hairline-divided — the mockup's .seg.
function PeriodControl({
  active,
  onChange,
}: {
  active?: string
  onChange: (updates: Record<string, string | undefined>) => void
}) {
  return (
    <div className="flex h-8 shrink-0 overflow-hidden rounded-md border border-border">
      {periodOptions.map(([label, days], index) => (
        <button
          key={label}
          type="button"
          aria-pressed={active === label}
          className={`px-3 text-[13px] transition-colors ${index ? "border-l border-border" : ""} ${
            active === label
              ? "bg-primary text-primary-foreground"
              : "text-foreground hover:bg-foreground/7"
          }`}
          onClick={() => {
            const to = new Date()
            const from = new Date()
            if (days) from.setDate(to.getDate() - days)
            else from.setDate(1)
            onChange({ from: from.toISOString(), to: to.toISOString() })
          }}
        >
          {label}
        </button>
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

  return <section ref={containerRef} className="relative rounded-lg border border-border bg-card p-4"><div className="mb-4 flex items-center gap-1.5"><h2 className="text-[15px]">Cost by day</h2><InfoTooltip text="Each square is a day; darker means more spend. Click a day to focus the whole page on it." />{selectedDayLabel && <span className="ml-2 inline-flex items-center gap-1 rounded-full border border-border px-2 py-0.5 font-mono text-[11px] text-muted-foreground">{selectedDayLabel}<button type="button" aria-label={`Clear ${selectedDayLabel} filter`} onClick={onClearSelectedDay} className="rounded-full px-0.5 text-foreground hover:bg-foreground/10">×</button></span>}</div><div className="flex gap-2"><div className="flex w-6 flex-col text-center font-mono text-[10px] text-muted-foreground"><div className="mb-1 h-4" aria-hidden="true" /><div className="grid flex-1 grid-rows-7 gap-1">{["M", "T", "W", "T", "F", "S", "S"].map((day, index) => <span key={`${day}-${index}`} className="flex items-center justify-center">{day}</span>)}</div></div><div className="min-w-0 flex-1"><div className="relative mb-1 h-4 font-mono text-[10px] text-muted-foreground">{heatmap.monthLabels.map(({ week, label }) => <span key={`${week}-${label}`} className="absolute" style={{ left: `${(week / 52) * 100}%` }}>{label}</span>)}</div><div className="grid grid-flow-col grid-cols-[repeat(52,minmax(0,1fr))] grid-rows-7 gap-1">{heatmap.days.map(({ iso, point }) => { const level = point?.costUsd ? Math.min(5, Math.ceil((point.costUsd / maxCost) * 5)) : 0; const selected = iso === selectedDay; return <button key={iso} onClick={() => onSelectDay(iso)} onMouseEnter={(event) => showTooltip(iso, point, event.clientX, event.clientY)} onMouseMove={(event) => showTooltip(iso, point, event.clientX, event.clientY)} onMouseLeave={() => setTooltip(null)} onFocus={(event) => { const rect = event.currentTarget.getBoundingClientRect(); showTooltip(iso, point, rect.left + rect.width / 2, rect.top + rect.height / 2) }} onBlur={() => setTooltip(null)} className={`aspect-square cursor-pointer rounded-sm border border-foreground/8 transition-shadow hover:ring-2 hover:ring-ring hover:ring-offset-1 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 ${selected ? "ring-2 ring-primary ring-offset-1" : ""}`} style={{ background: level ? `var(--heatmap-${level})` : "var(--muted)" }} /> })}</div></div></div>{tooltip && <div role="tooltip" className="pointer-events-none absolute z-10 rounded-lg border border-border bg-card px-2 py-1.5 text-xs text-foreground shadow-ds-md" style={{ left: tooltip.x, top: tooltip.y }}><div>{new Intl.DateTimeFormat(undefined, { weekday: "short", month: "short", day: "numeric", year: "numeric" }).format(new Date(`${tooltip.iso}T00:00:00`))}</div><div className="text-muted-foreground">{usdWhole.format(tooltip.point?.costUsd ?? 0)} · {tooltip.point?.runCount ?? 0} runs</div></div>}<div className="mt-3 flex items-center justify-end gap-1 text-[11px] text-muted-foreground">Less {Array.from({ length: 5 }, (_, index) => <i key={index} className="size-[11px] rounded-[2px]" style={{ background: `var(--heatmap-${index + 1})` }} />)} More</div></section>
}

function useModelData(costs?: CostOverview) { return useMemo(() => { const models = (costs?.seriesByModel ?? []).slice(0, 4); return (costs?.dailySeries ?? []).map((day, index) => ({ date: day.date, Other: Math.max(0, day.costUsd - models.reduce((sum, model) => sum + (model.dailySeries[index]?.costUsd ?? 0), 0)), ...Object.fromEntries(models.map((model) => [model.model, model.dailySeries[index]?.costUsd ?? 0])) })) }, [costs]) }
function useWorkflowCostComparisonData(drivers: AnalyticsCostDriver[]) { return useMemo(() => { const topDrivers = [...drivers].sort((a, b) => b.costUsd - a.costUsd).slice(0, 5); const topNames = topDrivers.map((driver) => driver.name); const topNameSet = new Set(topNames); const dataByDate = new Map<string, Record<string, string | number>>(); for (const driver of drivers) for (const point of driver.dailyCost) { const row = dataByDate.get(point.date) ?? { date: point.date, Other: 0, ...Object.fromEntries(topNames.map((name) => [name, 0])) }; if (topNameSet.has(driver.name)) row[driver.name] = Number(row[driver.name]) + point.costUsd; else row.Other = Number(row.Other) + point.costUsd; dataByDate.set(point.date, row) } return { data: [...dataByDate.values()].sort((a, b) => String(a.date).localeCompare(String(b.date))), topNames } }, [drivers]) }
// The two hairlines plus the three-label axes every plot on the page shares.
// `max` sets the vertical scale; passing it explicitly keeps the mid rule and
// the middle label on the same pixel. Returned as an array rather than a
// component: Recharts looks its axes up among its own children, so a wrapper
// component would hide them.
function plotFrame({
  max,
  ticks,
  dataKey = "date",
  formatValue,
}: {
  max: number
  ticks?: (string | number)[]
  dataKey?: string
  formatValue?: (value: number) => string
}) {
  const scale = yScale(max)
  return [
    <ReferenceLine key="mid" y={scale.mid} {...hairline} />,
    <ReferenceLine key="base" y={0} {...hairline} />,
    <XAxis key="x" dataKey={dataKey} ticks={ticks} tickFormatter={formatDate} {...axisProps} />,
    <YAxis
      key="y"
      width={34}
      domain={scale.domain}
      ticks={scale.ticks}
      tickFormatter={formatValue}
      {...axisProps}
    />,
  ]
}

function DailyCostChart({ costs, modelData }: { costs?: CostOverview; modelData: Record<string, string | number>[] }) {
  const labels = [...(costs?.seriesByModel ?? []).slice(0, 4).map((item) => item.model), "Other"]
  return (
    <ChartContainer config={chartConfig} className="h-64 w-full">
      <BarChart data={modelData} margin={plotMargin} barCategoryGap={2}>
        {plotFrame({
          max: stackedMax(modelData, labels),
          ticks: edgeTicks(modelData, (row) => String(row.date)),
          formatValue: (value) => usdWhole.format(value),
        })}
        <ChartTooltip content={<ChartTooltipContent />} />
        <ChartLegend content={<ChartLegendContent />} />
        {labels.map((label, index) => (
          <Bar key={label} dataKey={label} name={label} stackId="cost" fill={costSeriesColors[index % costSeriesColors.length]} />
        ))}
      </BarChart>
    </ChartContainer>
  )
}

function WorkflowCostComparisonChart({ drivers }: { drivers: AnalyticsCostDriver[] }) {
  const { data, topNames } = useWorkflowCostComparisonData(drivers)
  if (drivers.length === 0) return <EmptyPlot>No cost data for this period.</EmptyPlot>
  const keys = drivers.length > 5 ? [...topNames, "Other"] : topNames
  return (
    <ChartContainer config={chartConfig} className="h-64 w-full">
      <LineChart data={data} margin={plotMargin}>
        {plotFrame({
          max: seriesMax(data, keys),
          ticks: edgeTicks(data, (row) => String(row.date)),
          formatValue: (value) => usdWhole.format(value),
        })}
        <ChartTooltip content={<ChartTooltipContent />} />
        <ChartLegend content={<ChartLegendContent />} />
        {topNames.map((name, index) => (
          <Line key={name} type="monotone" dataKey={name} name={name} stroke={costSeriesColors[index % costSeriesColors.length]} dot={false} strokeWidth={2} />
        ))}
        {drivers.length > 5 && <Line type="monotone" dataKey="Other" name="Other" stroke="var(--ds-neutral-600)" dot={false} strokeWidth={2} />}
      </LineChart>
    </ChartContainer>
  )
}

const outcomeKeys = ["clean", "humanInTheLoop", "warning", "failed"] as const

function OutcomesChart({ effect }: { effect?: AnalyticsEffectiveness }) {
  const rows = effect?.outcomesByDay ?? []
  return (
    <ChartContainer config={chartConfig} className="h-64 w-full">
      <BarChart data={rows} margin={plotMargin} barCategoryGap={2}>
        {plotFrame({ max: stackedMax(rows, outcomeKeys), ticks: edgeTicks(rows, (row) => row.date) })}
        <ChartTooltip content={<ChartTooltipContent />} />
        <ChartLegend content={<ChartLegendContent />} />
        <Bar dataKey="clean" name="Clean" stackId="outcome" fill="var(--color-clean)" />
        <Bar dataKey="humanInTheLoop" name="Human on the loop" stackId="outcome" fill="var(--color-humanInTheLoop)" />
        <Bar dataKey="warning" name="Warning" stackId="outcome" fill="var(--color-warning)" />
        <Bar dataKey="failed" name="Failed" stackId="outcome" fill="var(--color-failed)" />
      </BarChart>
    </ChartContainer>
  )
}
function DeliveryFunnel({ effect }: { effect?: AnalyticsEffectiveness }) {
  const stages = [
    ["agentStarted", "Agent started"],
    ["prOpened", "PR opened"],
    ["prFinished", "PR finished"],
  ] as const

  return (
    <div className="grid gap-4 pt-2">
      {stages.map(([name, label], index) => {
        const value = effect?.funnel[name] ?? 0
        const previousStage = index ? stages[index - 1] : undefined
        const previousValue = previousStage ? effect?.funnel[previousStage[0]] ?? 0 : 0
        const conversion = previousStage && previousValue ? value / previousValue : undefined
        return (
          <div key={name}>
            <div className="mb-1.5 flex items-baseline justify-between gap-2 text-[13px]">
              <span>
                {label}
                {previousStage && conversion != null && (
                  <span className="text-[11px] text-muted-foreground">
                    {" "}· {formatPercent(conversion)} of {previousStage[1].toLowerCase()}
                  </span>
                )}
              </span>
              <span className="text-[16px] font-extrabold tabular-nums">{value}</span>
            </div>
            <div className="h-[18px] overflow-hidden rounded-sm bg-foreground/9">
              <div
                className="h-full bg-data"
                style={{ width: `${Math.min(100, (value / (effect?.funnel.agentStarted || 1)) * 100)}%` }}
              />
            </div>
          </div>
        )
      })}
    </div>
  )
}

function EmptyPlot({ children }: { children: ReactNode }) {
  return <p className="py-8 text-center text-sm text-muted-foreground">{children}</p>
}

// The mockup's .hbars: a fixed label column, a track that carries the bar, and
// a tabular number. Plain markup — a horizontal Recharts plot buys nothing here
// and cannot align its rows with the label column.
function HBars({
  rows,
  gridClassName,
  labelClassName = "",
}: {
  rows: { key: string; label: string; value: number; formatted: string; title?: string }[]
  // Full literal class strings: Tailwind only sees classes it can read verbatim.
  gridClassName: string
  labelClassName?: string
}) {
  const max = Math.max(...rows.map((row) => row.value), 0)
  return (
    <div className="grid gap-2.5 pt-2">
      {rows.map((row) => (
        <div
          key={row.key}
          title={row.title}
          className={`grid items-center gap-2 text-xs ${gridClassName}`}
        >
          <span className={`truncate ${labelClassName}`}>{row.label}</span>
          <span className="h-4 overflow-hidden rounded-sm bg-foreground/8">
            <span className="block h-full bg-data" style={{ width: `${max ? (row.value / max) * 100 : 0}%` }} />
          </span>
          <span className="text-right tabular-nums">{row.formatted}</span>
        </div>
      ))}
    </div>
  )
}
const ticketKeys = ["delivered", "inProgress", "failed"] as const

function TicketThroughputChart({ effect }: { effect?: AnalyticsEffectiveness }) {
  const rows = effect?.ticketsByDay ?? []
  if (!rows.length) return <EmptyPlot>No ticket data for this period.</EmptyPlot>
  return (
    <ChartContainer config={chartConfig} className="h-64 w-full">
      <BarChart data={rows} margin={plotMargin} barCategoryGap={2}>
        {plotFrame({ max: stackedMax(rows, ticketKeys), ticks: edgeTicks(rows, (row) => row.date) })}
        <ChartTooltip content={<ChartTooltipContent />} />
        <ChartLegend content={<ChartLegendContent />} />
        <Bar dataKey="delivered" name="Delivered" stackId="ticket" fill="var(--color-ticketDelivered)" />
        <Bar dataKey="inProgress" name="In progress" stackId="ticket" fill="var(--color-ticketInProgress)" />
        <Bar dataKey="failed" name="Failed" stackId="ticket" fill="var(--color-ticketFailed)" />
      </BarChart>
    </ChartContainer>
  )
}

function RunsPerTicketChart({ effect }: { effect?: AnalyticsEffectiveness }) {
  const buckets = effect?.runsPerTicket ?? []
  if (!buckets.length) return <EmptyPlot>No ticket data for this period.</EmptyPlot>
  return (
    <HBars
      gridClassName="grid-cols-[88px_minmax(0,1fr)_56px]"
      rows={buckets.map((bucket) => ({
        key: bucket.bucket,
        label: `${bucket.bucket} ${bucket.bucket === "1" ? "run" : "runs"}`,
        value: bucket.tickets,
        formatted: String(bucket.tickets),
      }))}
    />
  )
}

function TopTicketsByCostChart({ effect }: { effect?: AnalyticsEffectiveness }) {
  const tickets = effect?.topTicketsByCost ?? []
  if (!tickets.length) return <EmptyPlot>No ticket data for this period.</EmptyPlot>
  return (
    <HBars
      gridClassName="grid-cols-[110px_minmax(0,1fr)_64px]"
      labelClassName="font-mono"
      rows={tickets.map((ticket) => ({
        key: ticket.issueId,
        label: ticket.issueId,
        value: ticket.costUsd,
        formatted: usd.format(ticket.costUsd),
        // The detail the old Recharts tooltip carried, kept on the row itself.
        title: `${ticket.issueTitle} — ${usd.format(ticket.costUsd)} · ${ticket.runs} runs`,
      }))}
    />
  )
}
function CostPerMergedPrChart({ effect }: { effect?: AnalyticsEffectiveness }) {
  const rows = effect?.costPerMergedPr.weekly ?? []
  const average = effect?.costPerMergedPr.average ?? 0
  const scale = yScale(Math.max(seriesMax(rows, ["costPerMergedPr"]), average))
  return (
    <ChartContainer config={chartConfig} className="h-64 w-full">
      <LineChart data={rows} margin={plotMargin}>
        <ReferenceLine y={scale.mid} {...hairline} />
        <ReferenceLine y={0} {...hairline} />
        <XAxis
          dataKey="weekStart"
          ticks={edgeTicks(rows, (row) => row.weekStart)}
          tickFormatter={formatDate}
          {...axisProps}
        />
        <YAxis
          width={34}
          domain={scale.domain}
          ticks={scale.ticks}
          tickFormatter={(value) => usdWhole.format(value)}
          {...axisProps}
        />
        <ChartTooltip content={<ChartTooltipContent />} />
        <ChartLegend content={<ChartLegendContent />} />
        <ReferenceLine y={average} stroke="var(--ds-neutral-500)" strokeDasharray="4 4" />
        <Line type="monotone" dataKey="costPerMergedPr" name="Cost per merged PR" stroke="var(--color-costPerMergedPr)" dot={false} strokeWidth={2} />
      </LineChart>
    </ChartContainer>
  )
}
function CostDrivers({ drivers }: { drivers: AnalyticsCostDriver[] }) {
  return <section className="rounded-lg border border-border bg-card p-4"><PanelHeader title="Top cost drivers" description="By workflow — total spend and efficiency in the selected period." /><Table><TableHeader><TableRow><TableHead>Workflow</TableHead><TableHead className="text-right">Runs</TableHead><TableHead className="text-right">Success</TableHead><TableHead className="text-right">Cost</TableHead><TableHead className="text-right">Cost / merged PR</TableHead></TableRow></TableHeader><TableBody>{drivers.slice(0, 10).map((driver) => <TableRow key={driver.name}><TableCell className="font-medium">{driver.name}</TableCell><TableCell className="text-right tabular-nums">{driver.runs}</TableCell><TableCell className="text-right tabular-nums">{formatPercent(driver.successRate)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costUsd)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costPerMergedPr)}</TableCell></TableRow>)}</TableBody></Table></section>
}
const runsTableRightAlignedHeaders = new Set(["Cost", "Duration", "Start date"])
function RunsTable({ runs, page, canGoPrevious, canGoNext, onSelect, onPrevious, onNext }: { runs: TaskRunSummary[]; page: number; canGoPrevious: boolean; canGoNext: boolean; onSelect: (runId: string) => void; onPrevious: () => void; onNext: () => void }) { return <section className="rounded-lg border border-border bg-card p-4"><PanelHeader title="Runs" description="Every run in the selected period. Click a row for attempts, events and PRs." /><Table><TableHeader><TableRow>{["Status", "Ticket", "Model", "Factory/Workflow", "Cost", "Duration", "Start date"].map((label) => <TableHead key={label} className={runsTableRightAlignedHeaders.has(label) ? "text-right" : ""}>{label}</TableHead>)}</TableRow></TableHeader><TableBody>{runs.map((run) => <TableRow key={run.runId} className="cursor-pointer" onClick={() => onSelect(run.runId)}><TableCell><StatusBadge status={run.status} /></TableCell><TableCell className="font-mono">{run.issueId || "—"}</TableCell><TableCell>{run.model || "—"}</TableCell><TableCell>{run.factoryName || run.workflowName || "—"}</TableCell><TableCell className="text-right tabular-nums">{usd.format(run.estimatedCostUsd || 0)}</TableCell><TableCell className="text-right tabular-nums">{formatDuration(run.finishedAt ? run.finishedAt - run.startedAt : undefined)}</TableCell><TableCell className="text-right tabular-nums">{run.startedAt ? new Date(run.startedAt).toLocaleDateString() : "—"}</TableCell></TableRow>)}</TableBody></Table><div className="mt-3 flex items-center justify-center gap-3"><Button variant="outline" size="sm" disabled={!canGoPrevious} onClick={onPrevious}>Previous</Button><span className="text-sm text-muted-foreground">Page {page}</span><Button variant="outline" size="sm" disabled={!canGoNext} onClick={onNext}>Next</Button></div></section> }
function calculateDelta(current?: number | null, prior?: number | null) { return current == null || prior == null || prior === 0 ? undefined : (current - prior) / prior }
function KpiGroup({ title, columns = "sm:grid-cols-5", children }: { title: string; columns?: string; children: ReactNode }) { return <div><p className="kicker mb-2 font-semibold text-muted-foreground">{title}</p><div className={`grid grid-cols-2 gap-2 ${columns}`}>{children}</div></div> }
function Kpi({ label, value, change, good, cost, onClick, title }: { label: string; value?: string | number; change?: number; good?: boolean; cost?: boolean; onClick?: () => void; title?: string }) {
  const bad = cost ? (change ?? 0) > 0 : good ? (change ?? 0) < 0 : (change ?? 0) > 0
  const disabled = !onClick
  const button = (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className="grid h-full w-full min-w-0 grid-rows-[auto_1fr_auto] gap-1.5 rounded-lg border border-border bg-card p-3 text-left transition-colors enabled:hover:bg-foreground/4"
    >
      {/* Three rows — label, value, delta — so labels of different lengths
          never push the values out of line across the grid. */}
      <span className="flex items-start gap-1 text-pretty text-[11px] leading-[1.35] text-muted-foreground">
        {label}
        {title && <Info aria-hidden className="mt-px size-3.5 shrink-0 text-muted-foreground" />}
      </span>
      <span className="self-end text-[23px] font-extrabold leading-none tracking-[-0.02em] tabular-nums">{value ?? "—"}</span>
      {/* Always render the delta row, even when the API has no prior-period
          value to compare against, so every tile reserves the same space. */}
      <span
        className={`flex min-h-[15px] items-center gap-1 truncate whitespace-nowrap text-[11px] font-semibold ${
          change == null ? "invisible" : bad ? "text-primary" : "text-status-ok"
        }`}
      >
        {change != null ? (
          <>
            {change > 0 ? "↑" : "↓"} {Math.abs(change * 100).toFixed(1)}%{" "}
            <span className="font-normal text-muted-foreground">vs prior</span>
          </>
        ) : (
          " "
        )}
      </span>
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
// Panel header: a 15px Archivo-800 title and a 12px muted line saying what the
// panel shows. The info tooltip stays only where it carries more than the
// description does.
function PanelHeader({ title, description, info }: { title: string; description?: string; info?: string }) {
  return (
    <header className="mb-3 grid gap-0.5">
      <div className="flex items-center gap-1.5">
        <h2 className="text-[15px]">{title}</h2>
        {info && <InfoTooltip text={info} />}
      </div>
      {description && <p className="text-xs text-muted-foreground">{description}</p>}
    </header>
  )
}

function ChartCard({ title, description, info, children }: { title: string; description?: string; info?: string; children: ReactNode }) {
  return (
    <section className="rounded-lg border border-border bg-card p-4">
      <PanelHeader title={title} description={description} info={info} />
      {children}
    </section>
  )
}
