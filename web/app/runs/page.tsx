"use client"

import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { AlertCircle, RefreshCw, Search } from "lucide-react"
import { fetchTaskRunFilterOptions, fetchTaskRuns } from "@/lib/api"
import type { TaskRunAnalyticsFilters, TaskRunFilterOptions, TaskRunSummary } from "@/lib/types"
import {
  FilterSelect,
  RunDetailPanel,
  StatusBadge,
  formatCompactTokens,
  formatDurationMs,
  formatTime,
  formatUSD,
  runDurationMs,
  useRunDetails,
} from "@/components/task-run-analytics-view"
import { useCapabilities } from "@/hooks/use-capabilities"
import { PrimaryNavInline } from "@/components/primary-nav"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"

const PAGE_LIMIT = 50
const DAY_MS = 86_400_000

// Presets write plain from/to params (same names /analytics reads), so links
// between the two screens stay interchangeable.
const datePresets = [
  ["7d", 7],
  ["30d", 30],
  ["90d", 90],
  ["MTD", 0],
] as const

// Derives the active preset from the from/to params alone — never from the
// wall clock, which must not be read during render under Next's
// cacheComponents semantics.
function activeDatePreset(from?: string, to?: string): string | undefined {
  if (!from || !to) return undefined
  const fromDate = new Date(from)
  const toDate = new Date(to)
  if (Number.isNaN(fromDate.getTime()) || Number.isNaN(toDate.getTime())) return undefined
  // MTD is checked first: its span in days can collide with 7d/30d (e.g. on
  // the 8th of a month), but only MTD starts at local midnight on the 1st of
  // the same month as `to`, so the shapes are unambiguous.
  const isMonthStart =
    fromDate.getDate() === 1 &&
    fromDate.getHours() === 0 &&
    fromDate.getMinutes() === 0 &&
    fromDate.getSeconds() === 0 &&
    fromDate.getMilliseconds() === 0
  if (isMonthStart && fromDate.getMonth() === toDate.getMonth() && fromDate.getFullYear() === toDate.getFullYear()) {
    return "MTD"
  }
  const days = Math.round((toDate.getTime() - fromDate.getTime()) / DAY_MS)
  if (days === 7) return "7d"
  if (days === 30) return "30d"
  if (days === 90) return "90d"
  return undefined
}

function RunsScreen() {
  const router = useRouter()
  const pathname = usePathname()
  const params = useSearchParams()
  // Fetch filters are derived from individual params (not the whole query
  // string) so the client-side ticket filter can live in the URL without
  // refiring the runs request on every keystroke.
  const status = params.get("status") ?? undefined
  const workflow = params.get("workflow") ?? undefined
  const factory = params.get("factory") ?? undefined
  const from = params.get("from") ?? undefined
  const to = params.get("to") ?? undefined
  const ticketParam = params.get("ticket") ?? ""

  const filters = useMemo<TaskRunAnalyticsFilters>(() => ({
    limit: PAGE_LIMIT,
    analyticsEnabled: true,
    requiresPr: true,
    status,
    workflow,
    factory,
    from,
    to,
  }), [status, workflow, factory, from, to])

  const { canViewCosts } = useCapabilities()
  const [runs, setRuns] = useState<TaskRunSummary[]>([])
  const [options, setOptions] = useState<TaskRunFilterOptions | null>(null)
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null)
  const { details, loading: detailLoading, error: detailError } = useRunDetails(selectedRunId)
  const [ticketQuery, setTicketQuery] = useState(ticketParam)
  const optionsRef = useRef<TaskRunFilterOptions | null>(null)

  const selectedRun = useMemo(
    () => runs.find((run) => run.runId === selectedRunId) ?? null,
    [runs, selectedRunId]
  )

  // Every call (effect, Refresh, Load more) claims a fresh request id; a
  // response only applies its state if no newer request has started since,
  // so a stale fetch can never append rows or a cursor from old filters.
  const loadRequestId = useRef(0)

  const load = useCallback(async (cursor?: string, append = false) => {
    const requestId = ++loadRequestId.current
    const isStale = () => requestId !== loadRequestId.current
    setLoading(true)
    setError(null)
    try {
      const [runsData, optionsData] = await Promise.all([
        fetchTaskRuns({ ...filters, cursor }),
        optionsRef.current ? Promise.resolve(optionsRef.current) : fetchTaskRunFilterOptions(),
      ])
      if (isStale()) return
      setRuns((prev) => (append ? [...prev, ...runsData.runs] : runsData.runs))
      setNextCursor(runsData.nextCursor)
      if (!optionsRef.current) {
        optionsRef.current = optionsData
        setOptions(optionsData)
      }
      if (!append) setSelectedRunId(null)
    } catch (err) {
      if (isStale()) return
      setError(err instanceof Error ? err.message : "Unable to load runs")
    } finally {
      if (!isStale()) setLoading(false)
    }
  }, [filters])

  useEffect(() => {
    let cancelled = false
    queueMicrotask(() => {
      if (cancelled) return
      void load()
    })
    // No cleanup increment needed: the next load() call claims a fresh
    // request id, which makes any in-flight request stale.
    return () => { cancelled = true }
  }, [load])

  const replaceParams = useCallback((updates: Record<string, string | undefined>) => {
    const next = new URLSearchParams(params.toString())
    for (const [key, value] of Object.entries(updates)) {
      if (value) next.set(key, value)
      else next.delete(key)
    }
    const query = next.toString()
    router.replace(query ? `${pathname}?${query}` : pathname, { scroll: false })
  }, [params, pathname, router])

  const setDateRange = useCallback((days: number) => {
    const toDate = new Date()
    const fromDate = new Date(toDate)
    if (days) {
      fromDate.setDate(toDate.getDate() - days)
    } else {
      // MTD starts at local midnight on the 1st, not the current time-of-day,
      // so the first hours of the month are included.
      fromDate.setDate(1)
      fromDate.setHours(0, 0, 0, 0)
    }
    replaceParams({ from: fromDate.toISOString(), to: toDate.toISOString() })
  }, [replaceParams])

  const clearFilters = useCallback(() => {
    setTicketQuery("")
    router.replace(pathname, { scroll: false })
  }, [pathname, router])

  const setTicketFilter = useCallback((value: string) => {
    setTicketQuery(value)
    replaceParams({ ticket: value || undefined })
  }, [replaceParams])

  const activePreset = activeDatePreset(from, to)
  const hasActiveFilters = Boolean(status || workflow || factory || from || to || ticketQuery)

  const visibleRuns = useMemo(() => {
    const query = ticketQuery.trim().toLowerCase()
    if (!query) return runs
    return runs.filter((run) =>
      (run.issueId ?? "").toLowerCase().includes(query) ||
      (run.issueTitle ?? "").toLowerCase().includes(query)
    )
  }, [runs, ticketQuery])

  const columnCount = canViewCosts ? 7 : 5

  return (
    <main className="flex h-screen min-w-0 flex-col overflow-hidden bg-background">
      <header className="flex flex-col gap-3 border-b border-border px-4 py-3 lg:px-6">
        <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
          <div className="flex items-center gap-3">
            <PrimaryNavInline />
            <div className="h-6 w-px bg-border" aria-hidden />
            <div>
              <h1 className="text-lg font-semibold tracking-tight">Runs</h1>
              <p className="text-sm text-muted-foreground">Every agent run, including ones no longer on the board.</p>
            </div>
          </div>
          <div className="flex w-fit items-center gap-2">
            <Button variant="ghost" size="sm" onClick={clearFilters} disabled={!hasActiveFilters}>
              Clear filters
            </Button>
            <Button variant="outline" size="sm" className="gap-2" onClick={() => load()}>
              <RefreshCw className={cn("size-4", loading && "animate-spin")} />
              Refresh
            </Button>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex overflow-hidden rounded-md border">
            {datePresets.map(([label, days]) => (
              <Button
                key={label}
                variant="ghost"
                size="sm"
                className={cn("rounded-none", activePreset === label && "bg-accent")}
                aria-pressed={activePreset === label}
                onClick={() => setDateRange(days)}
              >
                {label}
              </Button>
            ))}
          </div>
          <div className="w-44">
            <FilterSelect label="Status" value={status} values={options?.statuses} onChange={(value) => replaceParams({ status: value })} />
          </div>
          <div className="w-44">
            <FilterSelect label="Workflow" value={workflow} values={options?.workflows} onChange={(value) => replaceParams({ workflow: value })} />
          </div>
          <div className="w-44">
            <FilterSelect label="Factory" value={factory} values={options?.factories} onChange={(value) => replaceParams({ factory: value })} />
          </div>
          <div className="relative w-56">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={ticketQuery}
              onChange={(event) => setTicketFilter(event.target.value)}
              placeholder="Filter loaded runs by ticket…"
              className="h-8 pl-8"
              aria-label="Filter loaded runs by ticket"
              title="Searches only the runs loaded so far — use Load more to keep searching"
            />
          </div>
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
                <TableHead>Ticket</TableHead>
                <TableHead>Workflow/Factory</TableHead>
                {canViewCosts && <TableHead>Model</TableHead>}
                {canViewCosts && <TableHead className="text-right">Cost</TableHead>}
                <TableHead className="text-right">Duration</TableHead>
                <TableHead className="text-right">Started</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visibleRuns.map((run) => (
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
                    {run.issueId ? (
                      <span className="block max-w-[260px] truncate">
                        {run.issueTitle ? `${run.issueId}: ${run.issueTitle}` : run.issueId}
                      </span>
                    ) : (
                      <span className="text-muted-foreground">None</span>
                    )}
                  </TableCell>
                  <TableCell>
                    <div className="min-w-[160px]">
                      <div className="font-medium">{run.workflowName || run.ownerDisplayName || "Unknown"}</div>
                      {run.factoryName && <div className="text-xs text-muted-foreground">{run.factoryName}</div>}
                    </div>
                  </TableCell>
                  {canViewCosts && (
                    <TableCell>{run.model || <span className="text-muted-foreground">Unknown</span>}</TableCell>
                  )}
                  {canViewCosts && (
                    <TableCell className="text-right tabular-nums" title={run.totalTokens != null ? `${formatCompactTokens(run.totalTokens)} tokens` : undefined}>
                      {formatUSD(run.estimatedCostUsd)}
                    </TableCell>
                  )}
                  <TableCell className="text-right tabular-nums text-muted-foreground">{formatDurationMs(runDurationMs(run))}</TableCell>
                  <TableCell className="text-right text-muted-foreground">{formatTime(run.startedAt)}</TableCell>
                </TableRow>
              ))}
              {!loading && visibleRuns.length === 0 && (
                <TableRow>
                  <TableCell colSpan={columnCount} className="h-32 text-center text-muted-foreground">
                    <Search className="mx-auto mb-2 size-5" />
                    {/* The ticket filter only searches loaded pages, so an
                        empty result is not authoritative while more pages
                        remain — say so instead of a false "no runs". */}
                    {ticketQuery.trim() && runs.length > 0
                      ? nextCursor
                        ? "No loaded runs match this ticket. Load more below to keep searching."
                        : "No runs match this ticket."
                      : "No runs match the current filters."}
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

export default function RunsPage() {
  return (
    <Suspense fallback={<div className="h-screen bg-background" aria-busy="true" />}>
      <RunsScreen />
    </Suspense>
  )
}
