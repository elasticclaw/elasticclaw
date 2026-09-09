const DS = window.ElasticClawDesignSystem_0b1de0
const D = window.EC_ANALYTICS

/* Charts from web/components/analytics-command-center.tsx. Recharts is replaced
   by SVG/CSS at the same shapes and the same titles/info strings.

   Series → color: the source's chartConfig (lines 76–85) hardcodes hex outside
   the token system — clean #0ca30c, humanInTheLoop #2a78d6, warning #fab219,
   failed #d03b3b, ticketInProgress #64748b — while only costPerMergedPr uses
   var(--chart-1). This kit routes all of them through the chart tokens at the
   same hues (green/blue/amber/red/slate), which is the point of the rebalance:
   same meaning, one saturation level. See readme.md → Analytics. */
const SERIES = {
  clean: { label: 'Clean', color: 'var(--chart-2)' },
  humanInTheLoop: { label: 'Human on the loop', color: 'var(--chart-1)' },
  warning: { label: 'Warning', color: 'var(--chart-3)' },
  failed: { label: 'Failed', color: 'var(--chart-4)' },
  ticketDelivered: { label: 'Delivered', color: 'var(--chart-2)' },
  ticketInProgress: { label: 'In progress', color: 'var(--chart-5)' },
  ticketFailed: { label: 'Failed', color: 'var(--chart-4)' },
}

const MONO = { fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)', color: 'var(--muted-foreground)' }
const fmtDate = (d) => new Date(d + 'T00:00:00Z').toLocaleString('en', { month: 'short', day: 'numeric', timeZone: 'UTC' })
/* Both formatters mirror analytics-command-center.tsx (~lines 86–94): Intl,
   USD, differing only in fraction digits. Hand-rolled string concat drops the
   thousands separator and puts three money formats on one page. */
const usdFmt = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 2 })
const usdWholeFmt = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 })
const usd = (n) => usdFmt.format(n)
const usdWhole = (n) => usdWholeFmt.format(n)

function Legend({ items }) {
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 12, marginTop: 10, justifyContent: 'center' }}>
      {items.map((i) => (
        <span key={i.label} style={{ display: 'flex', alignItems: 'center', gap: 5, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
          <span style={{ width: 8, height: 8, borderRadius: 2, background: i.color }} />{i.label}
        </span>
      ))}
    </div>
  )
}

/* Shared stacked-bar body: y axis, horizontal gridlines, one bar per day. */
function StackedBars({ data, keys, tickEvery = 5 }) {
  const totals = data.map((d) => keys.reduce((s, k) => s + d[k], 0))
  const max = Math.max(...totals, 1)
  const ticks = [max, Math.round(max / 2), 0]
  return (
    <div>
      <div style={{ display: 'flex', gap: 8, height: 220 }}>
        <div style={{ display: 'flex', flexDirection: 'column', justifyContent: 'space-between', width: 22, textAlign: 'right', ...MONO }}>
          {ticks.map((t) => <span key={t}>{t}</span>)}
        </div>
        <div style={{ position: 'relative', flex: 1, minWidth: 0 }}>
          {[0, 0.5, 1].map((g) => <span key={g} style={{ position: 'absolute', left: 0, right: 0, top: g * 100 + '%', height: 1, background: 'var(--border)' }} />)}
          <div style={{ position: 'relative', display: 'flex', alignItems: 'flex-end', gap: 3, height: '100%' }}>
            {data.map((d, i) => {
              const total = totals[i]
              return (
                <div key={d.date} title={fmtDate(d.date) + ' · ' + total} style={{ flex: 1, minWidth: 0, height: (total / max) * 100 + '%', display: 'flex', flexDirection: 'column-reverse', borderRadius: '2px 2px 0 0', overflow: 'hidden' }}>
                  {keys.map((k) => d[k] > 0 && <div key={k} style={{ height: (d[k] / total) * 100 + '%', background: SERIES[k].color }} />)}
                </div>
              )
            })}
          </div>
        </div>
      </div>
      <div style={{ display: 'flex', gap: 3, marginTop: 5, paddingLeft: 30 }}>
        {data.map((d, i) => (
          <span key={d.date} style={{ flex: 1, minWidth: 0, textAlign: 'center', ...MONO }}>{i % tickEvery === 0 ? fmtDate(d.date) : ''}</span>
        ))}
      </div>
      <Legend items={keys.map((k) => SERIES[k])} />
    </div>
  )
}

function OutcomesChart() {
  return <StackedBars data={D.outcomesByDay} keys={['clean', 'humanInTheLoop', 'warning', 'failed']} />
}

function TicketThroughputChart() {
  const data = D.ticketsByDay.map((d) => ({ date: d.date, ticketDelivered: d.delivered, ticketInProgress: d.inProgress, ticketFailed: d.failed }))
  return <StackedBars data={data} keys={['ticketDelivered', 'ticketInProgress', 'ticketFailed']} />
}

function DeliveryFunnel() {
  const stages = [['agentStarted', 'Agent started'], ['prOpened', 'PR opened'], ['prFinished', 'PR finished']]
  const top = D.funnel.agentStarted
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12, paddingTop: 12 }}>
      {stages.map(([key, label], i) => {
        const value = D.funnel[key]
        const prev = i ? D.funnel[stages[i - 1][0]] : 0
        return (
          <div key={key}>
            <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 4, fontSize: 'var(--text-sm)' }}>
              <span>{label}</span>
              <span style={{ fontVariantNumeric: 'tabular-nums' }}>{value}{i ? ' (' + ((value / prev) * 100).toFixed(1) + '%)' : ''}</span>
            </div>
            <div style={{ height: 20, borderRadius: 'var(--radius-sm)', background: 'var(--muted)' }}>
              <div style={{ height: '100%', width: Math.min(100, (value / top) * 100) + '%', borderRadius: 'var(--radius-sm)', background: 'var(--chart-1)' }} />
            </div>
          </div>
        )
      })}
    </div>
  )
}

/* Horizontal bars with a value label to the right — the source's
   layout="vertical" BarChart + LabelList position="right". */
function HBars({ rows, format = (v) => v, labelWidth = 40 }) {
  const max = Math.max(...rows.map((r) => r.value))
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 10, paddingTop: 12 }}>
      {rows.map((r) => (
        <div key={r.label} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span title={r.label} style={{ width: labelWidth, flexShrink: 0, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.label}</span>
          <div style={{ flex: 1, minWidth: 0, display: 'flex', alignItems: 'center', gap: 6 }}>
            <div style={{ height: 16, width: (r.value / max) * 100 + '%', background: 'var(--chart-1)', borderRadius: 4 }} title={r.title || ''} />
            <span style={{ flexShrink: 0, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', fontVariantNumeric: 'tabular-nums' }}>{format(r.value)}</span>
          </div>
        </div>
      ))}
    </div>
  )
}

function RunsPerTicketChart() {
  return <HBars rows={D.runsPerTicket.map((b) => ({ label: b.bucket, value: b.tickets }))} />
}

function TopTicketsByCostChart() {
  return (
    <HBars
      labelWidth={90}
      format={usd}
      rows={D.topTicketsByCost.map((t) => ({ label: t.issueId, value: t.costUsd, title: t.issueTitle + ' · ' + usd(t.costUsd) + ' · ' + t.runs + ' runs · ' + t.outcome }))}
    />
  )
}

/* Multi-series line chart in real pixel space (a stretched viewBox would
   distort the dots and stroke weight). */
function Lines({ data, series, yFormat = usdWhole, refLine }) {
  const W = 560, H = 220, L = 34, B = 22, T = 10
  const max = Math.max(...data.flatMap((d) => series.map((s) => d[s.key]))) * 1.1
  const x = (i) => L + (i / (data.length - 1)) * (W - L - 6)
  const y = (v) => H - B - (v / max) * (H - B - T)
  return (
    <div>
      <svg viewBox={'0 0 ' + W + ' ' + H} style={{ width: '100%', height: 220, display: 'block' }}>
        {[0, 0.5, 1].map((g) => {
          const v = max * (1 - g)
          return (
            <g key={g}>
              <line x1={L} x2={W - 6} y1={y(v)} y2={y(v)} stroke="var(--border)" strokeWidth="1" />
              <text x={L - 6} y={y(v) + 3.5} textAnchor="end" fill="var(--muted-foreground)" style={{ fontFamily: 'var(--font-mono)', fontSize: 9 }}>{yFormat(v)}</text>
            </g>
          )
        })}
        {refLine != null && <line x1={L} x2={W - 6} y1={y(refLine)} y2={y(refLine)} stroke="var(--muted-foreground)" strokeWidth="1" strokeDasharray="5 4" />}
        {series.map((s) => (
          <path key={s.key} d={data.map((d, i) => (i ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(d[s.key]).toFixed(1)).join(' ')} fill="none" stroke={s.color} strokeWidth="2" strokeLinejoin="round" />
        ))}
        {series.length === 1 && data.map((d, i) => <circle key={i} cx={x(i)} cy={y(d[series[0].key])} r="3" fill={series[0].color} />)}
        {data.map((d, i) => (i % Math.ceil(data.length / 6) === 0 || i === data.length - 1) && (
          <text key={'x' + i} x={x(i)} y={H - 6} textAnchor="middle" fill="var(--muted-foreground)" style={{ fontFamily: 'var(--font-mono)', fontSize: 9 }}>{fmtDate(d.date)}</text>
        ))}
      </svg>
      <Legend items={series.map((s) => ({ label: s.label, color: s.color }))} />
    </div>
  )
}

function CostPerMergedPrChart() {
  const data = D.costPerMergedPr.weekly.map((w) => ({ date: w.weekStart, cost: w.costPerMergedPr }))
  return <Lines data={data} refLine={D.costPerMergedPr.average} series={[{ key: 'cost', label: 'Cost per merged PR', color: 'var(--chart-1)' }]} />
}

function WorkflowCostComparisonChart() {
  return (
    <Lines
      data={D.workflowSeries}
      series={D.workflows.map((w, i) => ({ key: w, label: w, color: 'var(--chart-' + (i + 1) + ')' }))}
    />
  )
}

/* Stacked daily cost by model — chart-1..N in source order, "Other" last. */
function DailyCostChart() {
  const keys = [...D.models, 'Other']
  const totals = D.dailyByModel.map((d) => keys.reduce((s, k) => s + d[k], 0))
  const max = Math.max(...totals)
  return (
    <div>
      <div style={{ display: 'flex', gap: 8, height: 220 }}>
        <div style={{ display: 'flex', flexDirection: 'column', justifyContent: 'space-between', width: 26, textAlign: 'right', ...MONO }}>
          {[max, max / 2, 0].map((t, i) => <span key={i}>{usdWhole(t)}</span>)}
        </div>
        <div style={{ position: 'relative', flex: 1, minWidth: 0 }}>
          {[0, 0.5, 1].map((g) => <span key={g} style={{ position: 'absolute', left: 0, right: 0, top: g * 100 + '%', height: 1, background: 'var(--border)' }} />)}
          <div style={{ position: 'relative', display: 'flex', alignItems: 'flex-end', gap: 3, height: '100%' }}>
            {D.dailyByModel.map((d, i) => (
              <div key={d.date} title={fmtDate(d.date) + ' · ' + usdWhole(totals[i])} style={{ flex: 1, minWidth: 0, height: (totals[i] / max) * 100 + '%', display: 'flex', flexDirection: 'column-reverse', borderRadius: '2px 2px 0 0', overflow: 'hidden' }}>
                {keys.map((k, ki) => d[k] > 0 && <div key={k} style={{ height: (d[k] / totals[i]) * 100 + '%', background: 'var(--chart-' + (ki + 1) + ')' }} />)}
              </div>
            ))}
          </div>
        </div>
      </div>
      <div style={{ display: 'flex', gap: 3, marginTop: 5, paddingLeft: 34 }}>
        {D.dailyByModel.map((d, i) => <span key={d.date} style={{ flex: 1, minWidth: 0, textAlign: 'center', ...MONO }}>{i % 5 === 0 ? fmtDate(d.date) : ''}</span>)}
      </div>
      <Legend items={keys.map((k, i) => ({ label: k, color: 'var(--chart-' + (i + 1) + ')' }))} />
    </div>
  )
}

Object.assign(window, {
  OutcomesChart, TicketThroughputChart, DeliveryFunnel, RunsPerTicketChart,
  TopTicketsByCostChart, CostPerMergedPrChart, WorkflowCostComparisonChart, DailyCostChart,
  ANALYTICS_SERIES: SERIES, fmtAnalyticsDate: fmtDate, usdAnalytics: usd, usdWholeAnalytics: usdWhole,
})
