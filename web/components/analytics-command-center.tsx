"use client"

import { Fragment, memo, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { usePathname, useSearchParams } from "next/navigation"
import { ChevronDown, ChevronRight, X } from "lucide-react"
import type { DateRange } from "react-day-picker"
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
  fetchAnalyticsTickets,
  fetchGeneralStats,
  fetchTaskRunAnalyticsSummary,
  fetchTaskRunAttempts,
  fetchTaskRunEvents,
  fetchTaskRunFilterOptions,
  fetchTaskRunPRs,
  fetchTaskRun,
  fetchTaskRuns,
  fetchWorkspaces,
} from "@/lib/api"
import { cn } from "@/lib/utils"
import type {
  AnalyticsCostDriver,
  AnalyticsTicket,
  AnalyticsEffectiveness,
  CostOverview,
  GeneralStats,
  TaskRunAnalyticsFilters,
  TaskRunAnalyticsSummary,
  TaskRunFilterOptions,
  TaskRunSummary,
} from "@/lib/types"
import { MultiFilterSelect, selectedFilterValues, serializeFilterValues } from "@/components/multi-filter-select"
import { type DetailState, urlFilterKeys } from "@/lib/task-run-filters"
import { RunDetailPanel } from "@/components/run-detail-panel"
import { TicketDetailPanel } from "@/components/ticket-detail-panel"
import { ChartCard, DatePickerRange, KpiTile, RunStatusBadge, TicketStatusBadge } from "@/components/ds"
import { WorkflowName } from "@/components/workflow-name"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
} from "@/components/ui/select"
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
// Matches the heatmap's custom tooltip (bg-card/border/shadow-md) so every
// explanatory tooltip in this file shares one look, in both themes.
const tooltipContentClassName = "max-w-xs rounded-lg border bg-card text-foreground shadow-md"
const chartConfig = {
  clean: { label: "Clean", color: "var(--chart-2)" },
  humanInTheLoop: { label: "Human on the loop", color: "var(--chart-1)" },
  warning: { label: "Warning", color: "var(--chart-3)" },
  failed: { label: "Failed", color: "var(--chart-4)" },
  ticketDelivered: { label: "Delivered", color: "var(--chart-2)" },
  ticketInProgress: { label: "In progress", color: "var(--chart-5)" },
  ticketFailed: { label: "Failed", color: "var(--chart-4)" },
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

// Recharts colors legend item text with the series color by default. Our chart
// rules keep the swatch colored but render the label text in the muted
// foreground, so wrap it and let the Legend's own dot/line swatch carry the color.
function legendTextFormatter(value: string) {
  return <span className="text-muted-foreground">{value}</span>
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

function localDayRange(date: Date) {
  const from = new Date(date)
  from.setHours(0, 0, 0, 0)
  const to = new Date(date)
  to.setHours(23, 59, 59, 999)
  return { from: from.toISOString(), to: to.toISOString() }
}

function AnalyticsCommandCenterInner() {
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
  const [tickets, setTickets] = useState<AnalyticsTicket[]>([])
  const [ticketTotal, setTicketTotal] = useState(0)
  const [options, setOptions] = useState<TaskRunFilterOptions>()
  const [nextTicketCursor, setNextTicketCursor] = useState<string>()
  const [ticketCursorStack, setTicketCursorStack] = useState<(string | undefined)[]>([undefined])
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)
  const [selectedTicketId, setSelectedTicketId] = useState<string | null>(null)
  const [details, setDetails] = useState<DetailState | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [error, setError] = useState<string>()
  const loadAbortController = useRef<AbortController | null>(null)
  const loadRequestId = useRef(0)
  const optionsLoadedRef = useRef(false)

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
  const [selectedTicketCache, setSelectedTicketCache] = useState<{ issueId: string; ticket: AnalyticsTicket } | null>(null)
  const foundTicket = selectedTicketId ? tickets.find((ticket) => ticket.issueId === selectedTicketId) ?? null : null
  if (selectedTicketId && foundTicket && (selectedTicketCache?.issueId !== selectedTicketId || selectedTicketCache.ticket !== foundTicket)) {
    setSelectedTicketCache({ issueId: selectedTicketId, ticket: foundTicket })
  }
  const selectedTicket = selectedTicketId
    ? foundTicket ?? (selectedTicketCache?.issueId === selectedTicketId ? selectedTicketCache.ticket : null)
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
    async (cursor?: string, loadOptions: { append?: boolean; silent?: boolean; ticketCursor?: string } = {}) => {
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
        const ticketFilters = { ...effectiveFilters, cursor: loadOptions.ticketCursor }
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
          ticketsData,
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
          fetchAnalyticsTickets(ticketFilters, { signal: controller.signal }),
          optionsLoadedRef.current ? Promise.resolve(undefined) : fetchTaskRunFilterOptions({ signal: controller.signal }),
        ])
        if (controller.signal.aborted || requestId !== loadRequestId.current) return

        setSummary(summaryData)
        setCosts(costsData)
        setYearCosts(yearCostsData)
        setEffect(effectData)
        setStats(statsData)
        setDrivers(driversData)
        setRuns((currentRuns) => (append ? [...currentRuns, ...runsData.runs] : runsData.runs))
        setTickets(ticketsData.tickets)
        setNextTicketCursor(ticketsData.nextCursor)
        setTicketTotal(ticketsData.total)
        if (optionsData) {
          optionsLoadedRef.current = true
          setOptions(optionsData)
        }
        if (!append && !silent) {
          setSelectedRunId(null)
          setSelectedTicketId(null)
          setSelectedRunCache(null)
          setSelectedTicketCache(null)
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
    [filters]
  )

  const loadTickets = useCallback(async (ticketCursor?: string) => {
    const requestId = ++loadRequestId.current
    try {
      const effectiveFilters = { ...filters }
      if (!effectiveFilters.from && !effectiveFilters.to) {
        const to = new Date()
        const from = new Date(to)
        from.setDate(to.getDate() - 30)
        effectiveFilters.from = from.toISOString()
        effectiveFilters.to = to.toISOString()
      }
      const ticketsData = await fetchAnalyticsTickets({ ...effectiveFilters, cursor: ticketCursor })
      if (requestId !== loadRequestId.current) return
      setTickets(ticketsData.tickets)
      setNextTicketCursor(ticketsData.nextCursor)
      setTicketTotal(ticketsData.total)
    } catch (loadError) {
      if (requestId !== loadRequestId.current) return
      setError(loadError instanceof Error ? loadError.message : "Unable to load tickets")
    }
  }, [filters])

  useEffect(() => {
    queueMicrotask(() => {
      setTicketCursorStack([undefined])
      void load()
    })
    return () => {
      loadAbortController.current?.abort()
    }
  }, [load])

  const silentRefresh = useCallback(() => {
    void load(undefined, { append: false, silent: true, ticketCursor: ticketCursorStack[ticketCursorStack.length - 1] })
  }, [load, ticketCursorStack])

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

  const handleNextTicketPage = useCallback(() => {
    if (!nextTicketCursor) return
    setTicketCursorStack((currentStack) => [...currentStack, nextTicketCursor])
    void loadTickets(nextTicketCursor)
  }, [loadTickets, nextTicketCursor])

  const handlePreviousTicketPage = useCallback(() => {
    if (ticketCursorStack.length <= 1) return
    const nextStack = ticketCursorStack.slice(0, -1)
    setTicketCursorStack(nextStack)
    void loadTickets(nextStack[nextStack.length - 1])
  }, [loadTickets, ticketCursorStack])

  useEffect(() => {
    if (!selectedRunId) return

    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      setDetailLoading(true)
      setDetailError(null)
      setDetails(null)
      Promise.all([
        fetchTaskRun(selectedRunId),
        fetchTaskRunAttempts(selectedRunId),
        fetchTaskRunEvents(selectedRunId),
        fetchTaskRunPRs(selectedRunId),
      ])
        .then(([runData, attempts, events, prs]) => {
          if (!cancelled) {
            setSelectedRunCache({ runId: selectedRunId, run: runData.run })
            setDetails({ attempts: attempts.attempts, events: events.events, prs: prs.prs })
          }
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
  // Stable handlers so the memoized Heatmap (and its 364 memoized cells) only
  // re-render when the selection actually changes.
  const selectedDayRef = useRef<string | false | undefined>(undefined)
  selectedDayRef.current = selectedDay
  const selectDay = useCallback((day: string) => {
    setFilters(day === selectedDayRef.current ? { from: undefined, to: undefined } : isoDayRange(day))
  }, [setFilters])
  const clearSelectedDay = useCallback(() => setFilters({ from: undefined, to: undefined }), [setFilters])
  const modelData = useModelData(costs)

  return (
    <main className="h-full overflow-auto bg-background" data-selected-ticket={selectedTicketId ?? undefined}>
      <div className="mx-auto max-w-[1400px] space-y-5 p-5 lg:p-6">
        <header>
          <h1 className="text-2xl font-semibold tracking-tight">Analytics</h1>
        </header>
        <FilterBar filters={filters} options={options} workspaces={workspaceNames} onChange={setFilters} />
        {error && <p className="text-sm text-destructive">{error}</p>}

        <KpiStrip>
          <KpiGroupLabel title="Effectiveness" />
            <Kpi
              label="Runs"
              title="Task runs started in the selected period."
              value={summary?.totalRuns}
              good
              change={calculateDelta(summary?.totalRuns, summary?.prior?.totalRuns)}
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
          <KpiGroupLabel title="Cost" />
            <Kpi label="Total cost" title="Total AI spend of the runs in the selected period." value={usdWhole.format(totalCost)} change={costDelta} cost />
            <Kpi
              label="Cost per run"
              title="Total cost divided by the number of runs."
              value={usd.format(summary?.totalRuns ? totalCost / summary.totalRuns : 0)}
              change={calculateDelta(
                summary?.totalRuns ? totalCost / summary.totalRuns : undefined,
                priorCost && summary?.prior?.totalRuns ? priorCost / summary.prior.totalRuns : undefined
              )}
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
        </KpiStrip>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Run outcomes over time" stat={outcomesStat(effect)} info="Each bar is a day. Clean = delivered with no human help; Human on the loop = a human helped via the pull request; Warning = a human had to step in from the dashboard; Failed = nothing was delivered.">
            <OutcomesChart effect={effect} />
          </ChartCard>
          <ChartCard title="Delivery funnel" stat={`${effect?.funnel.agentStarted ?? 0} started · ${formatPercent(effect?.funnel.agentStarted ? (effect.funnel.prFinished / effect.funnel.agentStarted) : undefined)} end to end`} info="How many runs made it from the agent starting, to opening a pull request, to that pull request being finished (merged or closed). Percentages show the conversion from the previous stage.">
            <DeliveryFunnel effect={effect} />
          </ChartCard>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Ticket throughput" stat={ticketThroughputStat(effect)} info="Each bar is a day: how many distinct tickets had their first run that day, by how the ticket ended up. Delivered = at least one run delivered the work.">
            <TicketThroughputChart effect={effect} />
          </ChartCard>
          <ChartCard title="Runs per ticket" stat={`${effect?.runsPerTicket.reduce((sum, bucket) => sum + bucket.tickets, 0) ?? 0} tickets · ${effect?.runsPerTicket.reduce((sum, bucket) => sum + (bucket.bucket === "3" || bucket.bucket === "4+" ? bucket.tickets : 0), 0) ?? 0} needed 3+`} info="How many runs each ticket needed. A long tail of 3+ means lots of retries on the same tickets.">
            <RunsPerTicketChart effect={effect} />
          </ChartCard>
        </div>

        <TicketsTable
          tickets={tickets}
          total={ticketTotal}
          page={ticketCursorStack.length}
          canGoPrevious={ticketCursorStack.length > 1}
          canGoNext={Boolean(nextTicketCursor)}
          onSelect={setSelectedRunId}
          onSelectTicket={setSelectedTicketId}
          onPrevious={handlePreviousTicketPage}
          onNext={handleNextTicketPage}
        />

        <ChartCard title="Cost by day" stat={`${heatmap.days.length} days · ${usdWhole.format(yearCosts?.dailySeries.reduce((sum, point) => sum + point.costUsd, 0) ?? 0)} total`} info="Each square is a day; darker means more spend. Click a day to focus the whole page on it.">
        <Heatmap
          heatmap={heatmap}
          maxCost={maxHeatCost}
          selectedDay={selectedDay}
          onSelectDay={selectDay}
          onClearSelectedDay={clearSelectedDay}
        />
        </ChartCard>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Daily cost by model" stat={`${costs?.dailySeries.length ?? 0} days · ${usdWhole.format(totalCost)} total`} info="How much was spent per day, split by AI model.">
            <DailyCostChart costs={costs} modelData={modelData} />
          </ChartCard>
          <ChartCard title="Cost per merged PR" stat={`${effect?.costPerMergedPr.weekly.length ?? 0} weeks · avg ${usd.format(effect?.costPerMergedPr.average ?? 0)}`} info="Weekly average of what one merged pull request cost. The reference line is the period average.">
            <CostPerMergedPrChart effect={effect} />
          </ChartCard>
        </div>

        <div className="grid gap-5 lg:grid-cols-2">
          <ChartCard title="Workflow cost comparison" stat={`${drivers.length} workflows · ${usdWhole.format(drivers.reduce((sum, driver) => sum + driver.costUsd, 0))} total`} info="Daily spend of the most expensive workflows in the selected period, compared side by side.">
            <WorkflowCostComparisonChart drivers={drivers} />
          </ChartCard>
          <ChartCard title="Most expensive tickets" stat={`${effect?.topTicketsByCost.length ?? 0} tickets · ${usdWhole.format(effect?.topTicketsByCost.reduce((sum, ticket) => sum + ticket.costUsd, 0) ?? 0)} combined`} info="Where the money concentrates: the costliest tickets in the selected period (cost counts only this period's runs).">
            <TopTicketsByCostChart effect={effect} />
          </ChartCard>
        </div>
        <CostDrivers drivers={drivers} />
      </div>
      <RunDetailPanel
        runId={selectedRunId}
        run={selectedRun}
        details={details}
        loading={detailLoading}
        error={detailError}
        onClose={() => setSelectedRunId(null)}
      />
      <TicketDetailPanel
        ticket={selectedTicket}
        onClose={() => { setSelectedTicketId(null); setSelectedRunId(null) }}
        onOpenRun={(ticketRun) => setSelectedRunId(ticketRun.runId)}
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
      <SelectTrigger size="sm" className="w-full bg-input/30 text-xs hover:bg-input/50">
        <span className="truncate">
          <span className="text-muted-foreground">Workspace:</span> <span className={value ? "text-foreground" : "text-muted-foreground"}>{value ?? "All"}</span>
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

  const activeFilters: Array<{ key: "workspace" | (typeof selectFilters)[number][1]; label: string; value: string }> = [
    ...(filters.workspace ? [{ key: "workspace" as const, label: `Workspace: ${filters.workspace}`, value: filters.workspace }] : []),
    ...selectFilters.flatMap(([label, key]) => selectedFilterValues(filters[key]).map((value) => ({ key, label: `${label}: ${value}`, value }))),
  ]
  const removeFilterValue = (filter: { key: "workspace" | (typeof selectFilters)[number][1]; label: string; value: string }) => {
    if (filter.key === "workspace") {
      onChange({ workspace: undefined })
      return
    }
    const remaining = selectedFilterValues(filters[filter.key]).filter((value) => value !== filter.value)
    onChange({ [filter.key]: remaining.length > 0 ? serializeFilterValues(remaining) : undefined })
  }
  // No from/to in the URL means the default period — show it as such so the
  // trigger reads "Last 30 days" instead of "Select date range".
  const dateRange: DateRange | undefined = filters.from || filters.to ? { from: filters.from ? new Date(filters.from) : undefined, to: filters.to ? new Date(filters.to) : undefined } : undefined
  return (
    <div className="rounded-lg border bg-card p-2">
      {/* One flat flex row, like the kit: scope controls, rule, dimensions,
          Clear all — items wrap naturally instead of the dimensions forming
          their own block. */}
      <div className="flex flex-wrap items-center gap-2">
        <DatePickerRange value={dateRange} onChange={(range) => onChange({ from: range?.from ? localDayRange(range.from).from : undefined, to: range?.to ? localDayRange(range.to).to : undefined })} />
        <div className="w-[204px]">
          <WorkspaceSelect
            value={filters.workspace}
            workspaces={workspaces}
            onChange={(value) => onChange({ workspace: value })}
          />
        </div>
        <span className="mx-0.5 w-px self-stretch bg-border" aria-hidden="true" />
        {selectFilters.map(([label, key, values]) => (
          <div key={key} className="w-[158px]">
            <MultiFilterSelect
              label={label}
              value={filters[key]}
              values={values}
              onChange={(value) => onChange({ [key]: value })}
            />
          </div>
        ))}
        {activeFilters.length > 0 && <button type="button" className="ml-auto text-xs text-muted-foreground hover:text-foreground" onClick={() => onChange({ workspace: undefined, factory: undefined, workflow: undefined, repo: undefined, model: undefined })}>Clear all</button>}
      </div>
      {activeFilters.length > 0 && <div className="mt-2 flex flex-wrap items-center gap-1.5 border-t pt-2"><span className="text-[10px] font-medium uppercase tracking-[0.08em] text-muted-foreground">Filtering by</span>{activeFilters.map((filter) => <span key={`${filter.key}-${filter.value}`} title={filter.label} className="group inline-flex max-w-[220px] items-center gap-0.5 rounded-sm bg-secondary px-2 py-1 text-xs font-medium text-foreground"><span className="truncate">{filter.label}</span><button type="button" aria-label={`Remove ${filter.label}`} onClick={() => removeFilterValue(filter)} className="ml-0.5 flex cursor-pointer opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100"><X className="size-3" /></button></span>)}</div>}
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

type HeatmapPoint = ReturnType<typeof useHeatmap>["days"][number]["point"]

const HeatmapCell = memo(function HeatmapCell({ iso, point, level, selected, onSelectDay, onShowTooltip, onHideTooltip }: { iso: string; point: HeatmapPoint; level: number; selected: boolean; onSelectDay: (day: string) => void; onShowTooltip: (iso: string, point: HeatmapPoint, target: HTMLButtonElement) => void; onHideTooltip: () => void }) {
  return <button onClick={() => onSelectDay(iso)} onMouseEnter={(event) => onShowTooltip(iso, point, event.currentTarget)} onMouseLeave={onHideTooltip} onFocus={(event) => onShowTooltip(iso, point, event.currentTarget)} onBlur={onHideTooltip} className={`aspect-square cursor-pointer rounded-sm border border-black/5 transition-shadow hover:ring-2 hover:ring-ring hover:ring-offset-1 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 ${selected ? "ring-2 ring-primary ring-offset-1" : ""}`} style={{ background: level ? `var(--heatmap-${level})` : "var(--muted)" }} />
})

const Heatmap = memo(function Heatmap({ heatmap, maxCost, selectedDay, onSelectDay, onClearSelectedDay }: { heatmap: ReturnType<typeof useHeatmap>; maxCost: number; selectedDay?: string; onSelectDay: (day: string) => void; onClearSelectedDay: () => void }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [tooltip, setTooltip] = useState<{ iso: string; point: HeatmapPoint; x: number; y: number } | null>(null)
  const showTooltip = useCallback((iso: string, point: HeatmapPoint, target: HTMLButtonElement) => {
    const containerRect = containerRef.current?.getBoundingClientRect()
    const cellRect = target.getBoundingClientRect()
    if (containerRect) setTooltip((current) => current?.iso === iso ? current : { iso, point, x: cellRect.left + cellRect.width / 2 - containerRect.left + 12, y: cellRect.top + cellRect.height / 2 - containerRect.top + 12 })
  }, [])
  const hideTooltip = useCallback(() => setTooltip(null), [])
  const selectedDayLabel = selectedDay && new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" }).format(new Date(`${selectedDay}T00:00:00`))

  return <div ref={containerRef} className="relative">{selectedDayLabel && <div className="mb-4 flex items-center gap-1"><span className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">{selectedDayLabel}<button type="button" aria-label={`Clear ${selectedDayLabel} filter`} onClick={onClearSelectedDay} className="rounded-full px-0.5 text-foreground hover:bg-accent">×</button></span></div>}<div className="flex gap-2"><div className="flex w-6 flex-col text-center text-[10px] text-muted-foreground"><div className="mb-1 h-4" aria-hidden="true" /><div className="grid flex-1 grid-rows-7 gap-[3px]">{["M", "T", "W", "T", "F", "S", "S"].map((day, index) => <span key={`${day}-${index}`} className="flex items-center justify-center">{day}</span>)}</div></div><div className="min-w-0 flex-1"><div className="relative mb-1 h-4 text-[10px] text-muted-foreground">{heatmap.monthLabels.map(({ week, label }) => <span key={`${week}-${label}`} className="absolute" style={{ left: `${(week / 52) * 100}%` }}>{label}</span>)}</div><div className="grid grid-flow-col grid-cols-[repeat(52,minmax(0,1fr))] grid-rows-7 gap-[3px]">{heatmap.days.map(({ iso, point }) => { const level = point?.costUsd ? Math.min(5, Math.ceil((point.costUsd / maxCost) * 5)) : 0; return <HeatmapCell key={iso} iso={iso} point={point} level={level} selected={iso === selectedDay} onSelectDay={onSelectDay} onShowTooltip={showTooltip} onHideTooltip={hideTooltip} /> })}</div></div></div>{tooltip && <div role="tooltip" className="pointer-events-none absolute z-10 rounded-lg border bg-card px-2 py-1.5 text-xs text-foreground shadow-md" style={{ left: tooltip.x, top: tooltip.y }}><div>{new Intl.DateTimeFormat(undefined, { weekday: "short", month: "short", day: "numeric", year: "numeric" }).format(new Date(`${tooltip.iso}T00:00:00`))}</div><div className="text-muted-foreground">{usdWhole.format(tooltip.point?.costUsd ?? 0)} · {tooltip.point?.runCount ?? 0} runs</div></div>}<div className="mt-3 flex justify-end gap-1 text-xs text-muted-foreground">Less {Array.from({ length: 5 }, (_, index) => <i key={index} className="size-3 rounded-sm" style={{ background: `var(--heatmap-${index + 1})` }} />)} More</div></div>
})

function useModelData(costs?: CostOverview) { return useMemo(() => { const models = (costs?.seriesByModel ?? []).slice(0, 4); return (costs?.dailySeries ?? []).map((day, index) => ({ date: day.date, Other: Math.max(0, day.costUsd - models.reduce((sum, model) => sum + (model.dailySeries[index]?.costUsd ?? 0), 0)), ...Object.fromEntries(models.map((model) => [model.model, model.dailySeries[index]?.costUsd ?? 0])) })) }, [costs]) }
function useWorkflowCostComparisonData(drivers: AnalyticsCostDriver[]) { return useMemo(() => { const topDrivers = [...drivers].sort((a, b) => b.costUsd - a.costUsd).slice(0, 5); const topNames = topDrivers.map((driver) => driver.name); const topNameSet = new Set(topNames); const dataByDate = new Map<string, Record<string, string | number>>(); for (const driver of drivers) for (const point of driver.dailyCost) { const row = dataByDate.get(point.date) ?? { date: point.date, Other: 0, ...Object.fromEntries(topNames.map((name) => [name, 0])) }; if (topNameSet.has(driver.name)) row[driver.name] = Number(row[driver.name]) + point.costUsd; else row.Other = Number(row.Other) + point.costUsd; dataByDate.set(point.date, row) } return { data: [...dataByDate.values()].sort((a, b) => String(a.date).localeCompare(String(b.date))), topNames } }, [drivers]) }
function DailyCostChart({ costs, modelData }: { costs?: CostOverview; modelData: Record<string, string | number>[] }) { const labels = [...(costs?.seriesByModel ?? []).slice(0, 4).map((item) => item.model), "Other"]; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={modelData}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><Legend formatter={legendTextFormatter} />{labels.map((label, index) => <Bar key={label} dataKey={label} name={label} stackId="cost" fill={`var(--chart-${index + 1})`} />)}</BarChart></ChartContainer> }
function WorkflowCostComparisonChart({ drivers }: { drivers: AnalyticsCostDriver[] }) { const { data, topNames } = useWorkflowCostComparisonData(drivers); if (drivers.length === 0) return <p className="py-8 text-center text-sm text-muted-foreground">No cost data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><LineChart data={data}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><Legend formatter={legendTextFormatter} />{topNames.map((name, index) => <Line key={name} type="monotone" dataKey={name} name={name} stroke={`var(--chart-${index + 1})`} dot={false} strokeWidth={2} />)}{drivers.length > 5 && <Line type="monotone" dataKey="Other" name="Other" stroke="var(--muted-foreground)" dot={false} strokeWidth={2} />}</LineChart></ChartContainer> }
function OutcomesChart({ effect }: { effect?: AnalyticsEffectiveness }) { return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect?.outcomesByDay}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis allowDecimals={false} /><ChartTooltip content={<ChartTooltipContent />} /><Legend formatter={legendTextFormatter} /><Bar dataKey="clean" name="Clean" stackId="outcome" fill="var(--color-clean)" /><Bar dataKey="humanInTheLoop" name="Human on the loop" stackId="outcome" fill="var(--color-humanInTheLoop)" /><Bar dataKey="warning" name="Warning" stackId="outcome" fill="var(--color-warning)" /><Bar dataKey="failed" name="Failed" stackId="outcome" fill="var(--color-failed)" /></BarChart></ChartContainer> }
function DeliveryFunnel({ effect }: { effect?: AnalyticsEffectiveness }) {
  const stages = [
    ["agentStarted", "Agent started"],
    ["prOpened", "PR opened"],
    ["prFinished", "PR finished"],
  ] as const

  return <div className="space-y-3 pt-3">{stages.map(([name, label], index) => {
    const value = effect?.funnel[name] ?? 0
    const previous = index ? effect?.funnel[stages[index - 1][0]] ?? 0 : 0
    return <div key={name}><div className="mb-1 flex justify-between text-sm"><span>{label}</span><span className="tabular-nums">{value} {index ? `(${formatPercent(previous ? value / previous : undefined)})` : ""}</span></div><div className="h-5 rounded bg-muted"><div className="h-full rounded bg-chart-1" style={{ width: `${Math.min(100, (value / (effect?.funnel.agentStarted || 1)) * 100)}%` }} /></div></div>
  })}</div>
}
function TicketThroughputChart({ effect }: { effect?: AnalyticsEffectiveness }) { if (!effect?.ticketsByDay?.length) return <p className="py-8 text-center text-sm text-muted-foreground">No ticket data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect.ticketsByDay}><CartesianGrid vertical={false} /><XAxis dataKey="date" tickFormatter={formatDate} /><YAxis allowDecimals={false} /><ChartTooltip content={<ChartTooltipContent />} /><Legend formatter={legendTextFormatter} /><Bar dataKey="delivered" name="Delivered" stackId="ticket" fill="var(--color-ticketDelivered)" /><Bar dataKey="inProgress" name="In progress" stackId="ticket" fill="var(--color-ticketInProgress)" /><Bar dataKey="failed" name="Failed" stackId="ticket" fill="var(--color-ticketFailed)" /></BarChart></ChartContainer> }
function RunsPerTicketChart({ effect }: { effect?: AnalyticsEffectiveness }) { if (!effect?.runsPerTicket?.length) return <p className="py-8 text-center text-sm text-muted-foreground">No ticket data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect.runsPerTicket} layout="vertical" margin={{ right: 36 }}><CartesianGrid horizontal={false} /><XAxis type="number" allowDecimals={false} /><YAxis type="category" dataKey="bucket" width={40} /><Bar dataKey="tickets" fill="var(--chart-1)" radius={4}><LabelList dataKey="tickets" position="right" /></Bar></BarChart></ChartContainer> }
function TopTicketTooltip({ active, payload }: { active?: boolean; payload?: { payload?: AnalyticsEffectiveness["topTicketsByCost"][number] }[] }) { const ticket = payload?.[0]?.payload; if (!active || !ticket) return null; const outcome = ticket.outcome === "in_progress" ? "In progress" : ticket.outcome ? ticket.outcome.charAt(0).toUpperCase() + ticket.outcome.slice(1) : "—"; return <div className={`${tooltipContentClassName} px-3 py-2 text-xs`}><div className="font-medium">{ticket.issueTitle}</div><div className="text-muted-foreground">{usd.format(ticket.costUsd)} · {ticket.runs} runs · {outcome}</div></div> }
function TopTicketsByCostChart({ effect }: { effect?: AnalyticsEffectiveness }) { if (!effect?.topTicketsByCost?.length) return <p className="py-8 text-center text-sm text-muted-foreground">No ticket data for this period.</p>; return <ChartContainer config={chartConfig} className="h-64 w-full"><BarChart data={effect.topTicketsByCost} layout="vertical" margin={{ right: 36 }}><CartesianGrid horizontal={false} /><XAxis type="number" tickFormatter={(value) => usdWhole.format(value)} /><YAxis type="category" dataKey="issueId" width={90} interval={0} tickFormatter={(id) => (id.length > 14 ? id.slice(0, 13) + "…" : id)} /><ChartTooltip content={<TopTicketTooltip />} /><Bar dataKey="costUsd" fill="var(--chart-1)" radius={4}><LabelList dataKey="costUsd" position="right" formatter={(value: number) => usd.format(value)} /></Bar></BarChart></ChartContainer> }
function CostPerMergedPrChart({ effect }: { effect?: AnalyticsEffectiveness }) { return <ChartContainer config={chartConfig} className="h-64 w-full"><LineChart data={effect?.costPerMergedPr.weekly}><CartesianGrid vertical={false} /><XAxis dataKey="weekStart" tickFormatter={formatDate} /><YAxis /><ChartTooltip content={<ChartTooltipContent />} /><ReferenceLine y={effect?.costPerMergedPr.average} stroke="var(--muted-foreground)" /><Line type="monotone" dataKey="costPerMergedPr" name="Cost per merged PR" stroke="var(--color-costPerMergedPr)" dot={false} /></LineChart></ChartContainer> }
function CostDrivers({ drivers }: { drivers: AnalyticsCostDriver[] }) {
  return <ChartCard title="Top cost drivers" info="Where the money goes: total spend and efficiency per workflow in the selected period." stat={`${drivers.length} workflows · ${usdWhole.format(drivers.reduce((sum, driver) => sum + driver.costUsd, 0))} total`}><Table className="[&_th]:h-auto [&_th]:px-2 [&_th]:pt-0 [&_th]:pb-2 [&_th]:text-xs [&_td]:px-2 [&_td]:py-[9px]"><TableHeader><TableRow><TableHead className="min-w-[35ch]">Workflow</TableHead><TableHead className="text-right">Runs</TableHead><TableHead className="text-right">Success</TableHead><TableHead className="text-right">Cost</TableHead><TableHead className="text-right">Cost / merged PR</TableHead></TableRow></TableHeader><TableBody>{drivers.slice(0, 10).map((driver) => <TableRow key={driver.name}><TableCell className="font-medium min-w-[35ch]"><WorkflowName name={driver.name} /></TableCell><TableCell className="text-right tabular-nums">{driver.runs}</TableCell><TableCell className="text-right tabular-nums">{formatPercent(driver.successRate)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costUsd)}</TableCell><TableCell className="text-right tabular-nums">{usd.format(driver.costPerMergedPr)}</TableCell></TableRow>)}</TableBody></Table></ChartCard>
}
function TicketsTable({ tickets, total, page, canGoPrevious, canGoNext, onSelect, onSelectTicket, onPrevious, onNext }: { tickets: AnalyticsTicket[]; total: number; page: number; canGoPrevious: boolean; canGoNext: boolean; onSelect: (runId: string) => void; onSelectTicket: (ticketId: string) => void; onPrevious: () => void; onNext: () => void }) { const [expanded, setExpanded] = useState<Set<string>>(() => new Set()); const toggleExpanded = (issueId: string) => setExpanded((previous) => { const next = new Set(previous); if (next.has(issueId)) next.delete(issueId); else next.add(issueId); return next }); const delivered = tickets.filter((ticket) => ticket.status === "delivered").length; const awaitingReview = tickets.filter((ticket) => ticket.status === "pr_open").length; return <ChartCard title="Unique tickets" info="One row per ticket. Expand to see every run that served it; open a row for the business view, a run for the technical view." stat={<><span>{tickets.length} of {total} tickets</span><span className="ml-auto">{delivered} delivered · {awaitingReview} awaiting review</span></>}><Table className="[&_th]:h-auto [&_th]:px-2 [&_th]:pt-0 [&_th]:pb-2 [&_th]:text-xs [&_td]:px-2 [&_td]:py-[9px]"><TableHeader><TableRow>{["", "Status", "Ticket", "Requester", "Runs", "Cost", "Lead time", "Last activity"].map((label) => <TableHead key={label} className={label === "" ? "w-8" : ["Runs", "Cost", "Lead time", "Last activity"].includes(label) ? "text-right" : undefined}>{label}</TableHead>)}</TableRow></TableHeader><TableBody>{tickets.map((ticket) => <Fragment key={ticket.issueId}><TableRow className="cursor-pointer" onClick={() => onSelectTicket(ticket.issueId)}><TableCell><button type="button" aria-label={`Toggle ${ticket.issueId} runs`} aria-expanded={expanded.has(ticket.issueId)} onClick={(event) => { event.stopPropagation(); toggleExpanded(ticket.issueId) }}>{expanded.has(ticket.issueId) ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}</button></TableCell><TableCell><TicketStatusBadge status={ticket.status} /></TableCell><TableCell><div className="font-medium">{ticket.issueTitle || ticket.issueId}</div><div className="font-mono text-xs text-muted-foreground">{ticket.issueId} · {ticket.workflowName || ticket.source || "—"}</div></TableCell><TableCell>{ticket.requester || "—"}</TableCell><TableCell className="text-right tabular-nums">{ticket.runCount}</TableCell><TableCell className="text-right tabular-nums">{usd.format(ticket.cost)}</TableCell><TableCell className="text-right tabular-nums">{formatDuration(ticket.leadTime)}</TableCell><TableCell className="text-right tabular-nums">{ticket.lastActivity ? new Date(ticket.lastActivity).toLocaleDateString() : "—"}</TableCell></TableRow>{expanded.has(ticket.issueId) && ticket.runs.map((run, index) => { const prs = ticket.prs.filter((pr) => pr.runId === run.runId); return <TableRow key={run.runId} className="cursor-pointer bg-muted/30" onClick={() => onSelect(run.runId)}><TableCell /><TableCell><RunStatusBadge status={run.status} /></TableCell><TableCell><div>Try {index + 1}</div><div className="font-mono text-xs text-muted-foreground">{run.runId} · {run.model || "—"}</div></TableCell><TableCell>{prs.length ? prs.map((pr) => `#${pr.prNumber} ${pr.merged ? "merged" : pr.state}`).join(", ") : run.status === "failed" ? "Failed" : "—"}</TableCell><TableCell className="text-right tabular-nums">{run.attemptCount}</TableCell><TableCell className="text-right tabular-nums">{usd.format(run.cost)}</TableCell><TableCell className="text-right tabular-nums">{formatDuration(run.lastActivity - run.startedAt)}</TableCell><TableCell className="text-right tabular-nums">{new Date(run.startedAt).toLocaleDateString()}</TableCell></TableRow>})}</Fragment>)}</TableBody></Table><div className="mt-3 flex justify-center gap-3"><Button variant="outline" size="sm" disabled={!canGoPrevious} onClick={onPrevious}>Previous</Button><span className="text-sm text-muted-foreground">Page {page}</span><Button variant="outline" size="sm" disabled={!canGoNext} onClick={onNext}>Next</Button></div></ChartCard> }
function outcomesStat(effect?: AnalyticsEffectiveness) { const days = effect?.outcomesByDay ?? []; const total = days.reduce((sum, day) => sum + day.clean + day.humanInTheLoop + day.warning + day.failed, 0); return `${days.length} days · avg ${days.length ? (total / days.length).toFixed(1) : "0"} runs` }
function ticketThroughputStat(effect?: AnalyticsEffectiveness) { const days = effect?.ticketsByDay ?? []; const total = days.reduce((sum, day) => sum + day.delivered + day.inProgress + day.failed, 0); return `${days.length} days · avg ${days.length ? (total / days.length).toFixed(1) : "0"} tickets` }
function calculateDelta(current?: number | null, prior?: number | null) { return current == null || prior == null || prior === 0 ? undefined : (current - prior) / prior }
function KpiStrip({ children }: { children: ReactNode }) { const ref = useRef<HTMLDivElement>(null); const [columns, setColumns] = useState(6); useEffect(() => { const element = ref.current; if (!element) return; const measure = () => { const width = element.clientWidth; const fits = (count: number) => (width - (count - 1) * 8) / count >= 158; setColumns(fits(6) ? 6 : fits(3) ? 3 : 1) }; measure(); const observer = new ResizeObserver(measure); observer.observe(element); return () => observer.disconnect() }, []); return <div ref={ref} className="grid gap-2" style={{ gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))` }}>{children}</div> }
function KpiGroupLabel({ title }: { title: string }) { return <p className="col-span-full mb-0 text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">{title}</p> }
function Kpi({ label, value, change, good, cost, onClick, title }: { label: string; value?: string | number; change?: number; good?: boolean; cost?: boolean; onClick?: () => void; title?: string }) {
  const bad = cost ? (change ?? 0) > 0 : good ? (change ?? 0) < 0 : (change ?? 0) > 0
  return <KpiTile label={label} value={value} info={title} delta={change == null ? undefined : `${Math.abs(change * 100).toFixed(1)}%`} deltaDirection={change != null && change < 0 ? "down" : "up"} deltaTone={bad ? "bad" : "good"} onClick={onClick} className={onClick ? "cursor-pointer" : undefined} />
}
// Memoized: the home shell re-renders on every WS chunk while the hub is
// streaming, and this page (with recharts) must not re-render with it.
export const AnalyticsCommandCenter = memo(AnalyticsCommandCenterInner)
