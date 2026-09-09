const DS = window.ElasticClawDesignSystem_0b1de0
const { Button, Icon, Select } = DS
const D = window.EC_ANALYTICS

/* web/components/analytics-command-center.tsx — page shell, filter bar and the
   two KPI groups. Charts live in AnalyticsCharts.jsx, tables and the year
   heatmap in AnalyticsTables.jsx. Titles, info strings and column labels are
   lifted verbatim from the source. */

/* Built to the agent board card's anatomy (see ui_kits/web-dashboard/Board.jsx):
   a 12px header row separated by a hairline, the affordance icon right-aligned,
   the body in its own region, and a monospaced 10px stat line along the bottom
   edge. Same border, radius and --card fill; still no shadow. */
function ChartCard({ title, info, stat, children }) {
  /* No hover state: this is a plain section, like the source's ChartCard and
     like an agent board card — neither offers hover feedback. Only the KPI
     tiles do, because in the source they are buttons that apply filters. */
  return (
    <section
      style={{
        display: 'flex', flexDirection: 'column', minWidth: 0, overflow: 'hidden',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)', background: 'var(--card)',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: 12, borderBottom: '1px solid var(--border)' }}>
        <h2 style={{ margin: 0, minWidth: 0, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)' }}>{title}</h2>
        {info && (
          <span title={info} style={{ display: 'flex', flexShrink: 0, padding: 2, borderRadius: 'var(--radius-sm)', color: 'var(--muted-foreground)', cursor: 'help' }}>
            <Icon name="Info" size={14} />
          </span>
        )}
      </div>
      <div style={{ flex: 1, minHeight: 0, padding: 12 }}>{children}</div>
      {stat && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, borderTop: '1px solid var(--border)', padding: '4px 12px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
          <span>{stat.left}</span>
          {stat.right && <span style={{ marginLeft: 'auto', color: stat.tone === 'error' ? 'var(--text-error)' : undefined }}>{stat.right}</span>}
        </div>
      )}
    </section>
  )
}

/* All nine KPIs share ONE grid, with each group's name as a full-width subhead.
   Two separate auto-fit grids inside a fixed 6fr/3fr pair reflowed at different
   widths, so tile sizes and row counts disagreed.

   The column count is measured, not left to auto-fit: the groups hold 6 and 3
   tiles, so only 6, 3 and 1 columns divide both evenly. Free auto-fit lands on
   4 or 5 and leaves an orphan tile on its own row. */
const KPI_MIN = 158

function useKpiColumns(ref) {
  const [cols, setCols] = React.useState(6)
  React.useEffect(() => {
    const el = ref.current
    if (!el) return
    const measure = () => {
      const w = el.clientWidth
      const fits = (n) => (w - (n - 1) * 8) / n >= KPI_MIN
      setCols(fits(6) ? 6 : fits(3) ? 3 : 1)
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [ref])
  return cols
}

function KpiStrip({ children }) {
  const ref = React.useRef(null)
  const cols = useKpiColumns(ref)
  return (
    <div ref={ref} style={{ display: 'grid', gridTemplateColumns: 'repeat(' + cols + ', minmax(0, 1fr))', gap: 8, alignItems: 'stretch' }}>
      {children}
    </div>
  )
}

function KpiGroupLabel({ children }) {
  return (
    <p style={{ gridColumn: '1 / -1', margin: 0, fontSize: 'var(--text-12)', fontWeight: 'var(--weight-semibold)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)', color: 'var(--muted-foreground)' }}>{children}</p>
  )
}

/* KPI tile. Label is an uppercase micro caption, the value is monospaced (it is
   a machine number), and the delta leads with a direction arrow.
   Delta color follows the source: `bad = cost ? change > 0 : good ? change < 0 : change > 0`
   — so duration KPIs (no `good`, no `cost`) treat a rise as bad. */
function Kpi({ label, value, change, good, cost, title }) {
  const bad = cost ? (change ?? 0) > 0 : good ? (change ?? 0) < 0 : (change ?? 0) > 0
  const up = (change ?? 0) > 0
  const [hover, setHover] = React.useState(false)
  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex', height: '100%', minWidth: 0, flexDirection: 'column', gap: 4,
        border: '1px solid ' + (hover ? 'var(--ring)' : 'var(--border)'),
        borderRadius: 'var(--radius-lg)', background: 'var(--card)', padding: '10px 12px', textAlign: 'left',
        transition: 'border-color var(--duration-fast)',
      }}
    >
      <p style={{ display: 'flex', margin: 0, alignItems: 'flex-start', gap: 4, minWidth: 0, fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)', lineHeight: 1.3, color: 'var(--muted-foreground)' }}>
        {/* overflowWrap: the unbreakable "ready→merge" must break rather than
            spill past the tile at 6-up widths. */}
        <span style={{ minWidth: 0, overflowWrap: 'anywhere' }}>{label}</span>
        {title && <span title={title} style={{ display: 'flex', flexShrink: 0, cursor: 'help' }}><Icon name="Info" size={12} /></span>}
      </p>
      <div style={{ marginTop: 'auto', display: 'flex', flexDirection: 'column', gap: 1 }}>
        <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-medium)', letterSpacing: 'var(--tracking-tight)', fontVariantNumeric: 'tabular-nums' }}>{value ?? '—'}</span>
        {/* One line: the strip snaps to a column count that keeps tiles ≥158px,
            which holds arrow + percentage + basis. Ellipsis is the floor. */}
        <span style={{ display: 'flex', alignItems: 'center', gap: 3, minWidth: 0, overflow: 'hidden', whiteSpace: 'nowrap', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', visibility: change == null ? 'hidden' : 'visible', color: bad ? 'var(--text-error)' : 'var(--green-400)' }}>
          <Icon name={up ? 'ArrowUpRight' : 'ArrowDownRight'} size={11} />
          {change != null ? Math.abs(change * 100).toFixed(1) + '%' : ''}
          <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', color: 'var(--muted-foreground)' }}>vs prior</span>
        </span>
      </div>
    </div>
  )
}

/* Reorganized from the source's single undifferentiated row into two zones:
   SCOPE (what period and workspace am I looking at) and DIMENSIONS (how is it
   sliced), separated by a rule, with whatever is active restated as removable
   chips underneath. The four preset buttons are folded into DatePicker. */
const DIMENSIONS = [
  ['Factory', 'factory', ['bug-lane', 'docs-queue', 'release']],
  ['Workflow', 'workflow', D.workflows],
  ['Repo', 'repo', ['elasticclaw/elasticclaw', 'elasticclaw/docs']],
  ['Model', 'model', D.models],
]

function FilterBar() {
  const { DatePicker, Tag } = DS
  const [period, setPeriod] = React.useState({ from: '2026-07-17', to: '2026-08-15' })
  const [workspace, setWorkspace] = React.useState('')
  const [dims, setDims] = React.useState({ workflow: 'linear-story' })
  const active = [
    ...(workspace ? [{ key: 'workspace', label: 'Workspace: ' + workspace, clear: () => setWorkspace('') }] : []),
    ...DIMENSIONS.filter(([, key]) => dims[key]).map(([label, key]) => ({
      key, label: label + ': ' + dims[key], clear: () => setDims((p) => ({ ...p, [key]: '' })),
    })),
  ]
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8, border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)', padding: 8 }}>
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8 }}>
        <DatePicker value={period} onChange={setPeriod} today="2026-08-15" />
        <div style={{ width: 204 }}>
          <Select size="sm" value={workspace} onChange={setWorkspace} placeholder="Workspace: All" options={['elasticclaw', 'docs', 'support']} />
        </div>
        <span style={{ alignSelf: 'stretch', width: 1, background: 'var(--border)', margin: '0 2px' }} />
        {DIMENSIONS.map(([label, key, options]) => (
          <div key={key} style={{ width: 158 }}>
            <Select
              size="sm"
              value={dims[key] || ''}
              onChange={(v) => setDims((p) => ({ ...p, [key]: v }))}
              placeholder={label + ': All'}
              options={options}
            />
          </div>
        ))}
        {active.length > 0 && (
          <button
            type="button"
            onClick={() => { setWorkspace(''); setDims({}) }}
            style={{ marginLeft: 'auto', border: 0, background: 'none', cursor: 'pointer', fontFamily: 'var(--font-sans)', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}
          >Clear all</button>
        )}
      </div>
      {active.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 6, borderTop: '1px solid var(--border)', paddingTop: 8 }}>
          <span style={{ fontSize: 'var(--text-2xs)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)', color: 'var(--muted-foreground)' }}>Filtering by</span>
          {active.map((a) => <Tag key={a.key} size="md" onRemove={a.clear}>{a.label}</Tag>)}
        </div>
      )}
    </div>
  )
}

function AnalyticsScreen() {
  const k = D.kpis
  return (
    <main style={{ flex: 1, minWidth: 0, height: '100%', overflowY: 'auto', background: 'var(--background)' }}>
      <div style={{ margin: '0 auto', maxWidth: 1400, padding: 24, display: 'flex', flexDirection: 'column', gap: 20 }}>
        <header><h1 style={{ margin: 0, fontSize: 'var(--text-2xl)', fontWeight: 'var(--weight-semibold)', letterSpacing: 'var(--tracking-tight)' }}>Analytics</h1></header>

        <FilterBar />

        <KpiStrip>
          <KpiGroupLabel>Effectiveness</KpiGroupLabel>
          <Kpi label="Runs" title="Task runs started in the selected period." value={k.runs.value} change={k.runs.change} good />
          <Kpi label="Unique tickets" title="Distinct tickets worked on in the selected period. A ticket may have several runs." value={k.uniqueTickets.value} change={k.uniqueTickets.change} good />
          <Kpi label="Success rate (runs)" title="Of the runs that finished, the share that delivered their work (pull request merged or closed) — with or without human help." value={k.successRuns.value} change={k.successRuns.change} good />
          <Kpi label="Success rate (unique tickets)" title="Of the distinct tickets with at least one finished run, the share where at least one run delivered (clean, human on the loop, or warning)." value={k.successTickets.value} change={k.successTickets.change} good />
          <Kpi label="Avg ticket to PR" title="Average time from the ticket being created to the PR being opened (ready for review)." value={k.ticketToPr.value} change={k.ticketToPr.change} />
          <Kpi label="Avg ready→merge" title="Average business-hours time (your timezone) from ready-for-review to merge." value={k.readyToMerge.value} change={k.readyToMerge.change} />
          <KpiGroupLabel>Cost</KpiGroupLabel>
          <Kpi label="Total cost" title="Total AI spend of the runs in the selected period." value={k.totalCost.value} change={k.totalCost.change} cost />
          <Kpi label="Cost per run" title="Total cost divided by the number of runs." value={k.costPerRun.value} change={k.costPerRun.change} cost />
          <Kpi label="Cost per unique ticket" title="Total cost divided by the number of distinct tickets worked on in the period." value={k.costPerTicket.value} change={k.costPerTicket.change} cost />
        </KpiStrip>

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 20 }}>
          <ChartCard title="Run outcomes over time" stat={{ left: D.outcomesByDay.length + ' days', right: D.outcomesByDay.reduce((s, d) => s + d.clean + d.humanInTheLoop + d.warning + d.failed, 0) + ' runs' }} info="Each bar is a day. Clean = delivered with no human help; Human on the loop = a human helped via the pull request; Warning = a human had to step in from the dashboard; Failed = nothing was delivered."><OutcomesChart /></ChartCard>
          <ChartCard title="Delivery funnel" stat={{ left: D.funnel.agentStarted + ' started', right: ((D.funnel.prFinished / D.funnel.agentStarted) * 100).toFixed(1) + '% end to end' }} info="How many runs made it from the agent starting, to opening a pull request, to that pull request being finished (merged or closed). Percentages show the conversion from the previous stage."><DeliveryFunnel /></ChartCard>
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 20 }}>
          <ChartCard title="Ticket throughput" stat={{ left: D.ticketsByDay.length + ' days', right: D.ticketsByDay.reduce((s, d) => s + d.delivered + d.inProgress + d.failed, 0) + ' tickets' }} info="Each bar is a day: how many distinct tickets had their first run that day, by how the ticket ended up. Delivered = at least one run delivered the work."><TicketThroughputChart /></ChartCard>
          <ChartCard title="Runs per ticket" stat={{ left: D.runsPerTicket.reduce((s, b) => s + b.tickets, 0) + ' tickets', right: D.runsPerTicket[2].tickets + ' needed 3+' }} info="How many runs each ticket needed. A long tail of 3+ means lots of retries on the same tickets."><RunsPerTicketChart /></ChartCard>
        </div>

        <TicketsTable />
        <CostByDayHeatmap />

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 20 }}>
          <ChartCard title="Daily cost by model" stat={{ left: D.models.length + ' models', right: window.usdWholeAnalytics(D.dailyByModel.reduce((s, d) => s + D.models.reduce((m, k) => m + d[k], 0) + d.Other, 0)) + ' total' }} info="How much was spent per day, split by AI model."><DailyCostChart /></ChartCard>
          <ChartCard title="Cost per merged PR" stat={{ left: D.costPerMergedPr.weekly.length + ' weeks', right: 'avg ' + window.usdAnalytics(D.costPerMergedPr.average) }} info="Weekly average of what one merged pull request cost. The reference line is the period average."><CostPerMergedPrChart /></ChartCard>
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 20 }}>
          <ChartCard title="Workflow cost comparison" stat={{ left: D.workflows.length + ' workflows', right: 'top: ' + D.workflows[0] }} info="Daily spend of the most expensive workflows in the selected period, compared side by side."><WorkflowCostComparisonChart /></ChartCard>
          <ChartCard title="Most expensive tickets" stat={{ left: D.topTicketsByCost.length + ' tickets', right: window.usdAnalytics(D.topTicketsByCost.reduce((s, t) => s + t.costUsd, 0)) + ' combined' }} info="Where the money concentrates: the costliest tickets in the selected period (cost counts only this period's runs)."><TopTicketsByCostChart /></ChartCard>
        </div>

        <CostDrivers />
      </div>
    </main>
  )
}

Object.assign(window, { AnalyticsScreen, AnalyticsChartCard: ChartCard, AnalyticsKpi: Kpi, AnalyticsKpiStrip: KpiStrip })
