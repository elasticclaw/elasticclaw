const DS = window.ElasticClawDesignSystem_0b1de0
const { Button, Badge, Icon } = DS
const D = window.EC_ANALYTICS
const usd = window.usdAnalytics
const CAPTION_SM = { fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)' }
const pct = (v) => (v * 100).toFixed(1) + '%'

/* Same anatomy as the agent board card and the chart cards: hairline-separated
   12px header with the affordance on the right, body region, monospaced 10px
   stat line on the bottom edge. */
function Panel({ title, info, sub, stat, extra, children }) {
  const { Icon } = DS
  return (
    <section
      style={{
        position: 'relative', display: 'flex', flexDirection: 'column', overflow: 'hidden',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)', background: 'var(--card)',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: 12, borderBottom: '1px solid var(--border)' }}>
        <h2 style={{ margin: 0, flexShrink: 0, fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)' }}>{title}</h2>
        {sub && <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{sub}</span>}
        {extra}
        {info && (
          <span title={info} style={{ display: 'flex', flexShrink: 0, marginLeft: 'auto', padding: 2, color: 'var(--muted-foreground)', cursor: 'help' }}>
            <Icon name="Info" size={14} />
          </span>
        )}
      </div>
      <div style={{ padding: 12 }}>{children}</div>
      {stat && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, borderTop: '1px solid var(--border)', padding: '4px 12px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
          <span>{stat.left}</span>
          {stat.right && <span style={{ marginLeft: 'auto' }}>{stat.right}</span>}
        </div>
      )}
    </section>
  )
}

const TH = (right) => ({ textAlign: right ? 'right' : 'left', padding: '0 8px 8px', borderBottom: '1px solid var(--border)', fontSize: 'var(--text-12)', fontWeight: 'var(--weight-medium)', color: 'var(--muted-foreground)', whiteSpace: 'nowrap' })
const TD = (right) => ({ textAlign: right ? 'right' : 'left', padding: '9px 8px', borderBottom: '1px solid var(--border)', fontSize: 'var(--text-sm)', fontVariantNumeric: 'tabular-nums' })

/* Unique tickets, not runs: one row per ticket, and the runs that served it
   hang under it as child rows. Two drill-downs from here — the row opens the
   business view of the ticket, a child row opens the technical view of a run.
   Status is derived in analytics-data.js (a merged PR delivers; open PRs with
   nothing merged is the PR_OPEN state). */
function TicketsTable() {
  const headers = [['Status'], ['Ticket'], ['Requester'], ['Runs', true], ['Cost', true], ['Lead time', true], ['Last activity', true]]
  const [expanded, setExpanded] = React.useState(() => new Set())
  const [openTicket, setOpenTicket] = React.useState(null)
  const [openRun, setOpenRun] = React.useState(null)
  const [hoverId, setHoverId] = React.useState(null)
  const tickets = D.tickets
  const ticket = tickets.find((t) => t.issueId === openTicket) || null

  const toggle = (id) => setExpanded((prev) => {
    const next = new Set(prev)
    next.has(id) ? next.delete(id) : next.add(id)
    return next
  })

  const delivered = tickets.filter((t) => t.status === 'delivered').length
  const prOpen = tickets.filter((t) => t.status === 'pr_open').length

  return (
    <Panel
      title="Unique tickets"
      info="One row per ticket. Expand to see every run that served it; open a row for the business view, a run for the technical view."
      stat={{ left: tickets.length + ' of 96 tickets', right: delivered + ' delivered · ' + prOpen + ' awaiting review' }}
    >
      <table style={{ width: '100%', borderCollapse: 'collapse' }}>
        <thead><tr>{headers.map(([h, r]) => <th key={h} style={{ ...TH(r), paddingLeft: h === 'Status' ? 30 : 8 }}>{h}</th>)}</tr></thead>
        <tbody>
          {tickets.map((t) => {
            const isOpen = expanded.has(t.issueId)
            const runs = t.runIds.map((id) => D.runsTable.find((r) => r.runId === id))
            return (
              <React.Fragment key={t.issueId}>
                <tr
                  role="button" tabIndex={0}
                  onClick={() => setOpenTicket(t.issueId)}
                  onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpenTicket(t.issueId) } }}
                  onMouseEnter={() => setHoverId(t.issueId)}
                  onMouseLeave={() => setHoverId(null)}
                  style={{ cursor: 'pointer', background: t.issueId === openTicket || t.issueId === hoverId ? 'var(--accent)' : 'transparent' }}
                >
                  <td style={{ ...TD(), paddingLeft: 4, whiteSpace: 'nowrap' }}>
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                      <button
                        type="button"
                        aria-label={(isOpen ? 'Collapse' : 'Expand') + ' runs for ' + t.issueId}
                        aria-expanded={isOpen}
                        onClick={(e) => { e.stopPropagation(); toggle(t.issueId) }}
                        style={{ display: 'inline-flex', alignItems: 'center', justifyContent: 'center', width: 22, height: 22, flexShrink: 0, border: 0, borderRadius: 'var(--radius-sm)', background: 'none', color: 'var(--muted-foreground)', cursor: 'pointer', padding: 0, transform: isOpen ? 'rotate(90deg)' : 'none', transition: 'transform var(--duration-fast) var(--ease-default)' }}
                      >
                        <Icon name="ChevronRight" size={14} />
                      </button>
                      <window.TicketStatusBadge status={t.status} />
                    </span>
                  </td>
                  <td style={{ ...TD(), minWidth: 0 }}>
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
                      <span style={{ fontWeight: 'var(--weight-medium)' }}>{t.issueTitle}</span>
                      <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{t.issueId} · {t.workflowName}</span>
                    </div>
                  </td>
                  <td style={{ ...TD(), color: 'var(--muted-foreground)', whiteSpace: 'nowrap' }}>{t.requester}</td>
                  <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{t.runCount}</td>
                  <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{usd(t.cost)}</td>
                  <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{t.leadTime || '—'}</td>
                  <td style={{ ...TD(true), fontFamily: 'var(--font-mono)', color: 'var(--muted-foreground)', whiteSpace: 'nowrap' }}>{t.lastActivity}</td>
                </tr>
                {isOpen && runs.map((run, i) => (
                  <tr
                    key={run.runId}
                    role="button" tabIndex={0}
                    onClick={() => setOpenRun(run.runId)}
                    onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpenRun(run.runId) } }}
                    onMouseEnter={() => setHoverId(run.runId)}
                    onMouseLeave={() => setHoverId(null)}
                    style={{ cursor: 'pointer', background: run.runId === openRun || run.runId === hoverId ? 'var(--accent)' : 'var(--muted)' }}
                  >
                    <td style={{ ...TD(), paddingLeft: 30, whiteSpace: 'nowrap' }}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                        <span style={{ ...CAPTION_SM, color: 'var(--muted-foreground)' }}>Try {i + 1}</span>
                        <window.RunStatusBadge status={run.status} />
                      </span>
                    </td>
                    <td style={{ ...TD(), fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{run.runId} · {run.model}</td>
                    <td style={{ ...TD(), fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
                      {run.prs.length ? run.prs.map((p) => '#' + p.prNumber + ' ' + p.state).join(', ') : run.failureType ? window.formatRunLabel(run.failureType) : '—'}
                    </td>
                    <td style={{ ...TD(true), fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{run.attempts.length}</td>
                    <td style={{ ...TD(true), fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)' }}>{usd(run.cost)}</td>
                    <td style={{ ...TD(true), fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)' }}>{run.duration}</td>
                    <td style={{ ...TD(true), fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)', whiteSpace: 'nowrap' }}>{run.startedAt}</td>
                  </tr>
                ))}
              </React.Fragment>
            )
          })}
        </tbody>
      </table>
      <window.TicketDetailPanel ticket={ticket} onClose={() => setOpenTicket(null)} onOpenRun={(run) => setOpenRun(run.runId)} />
      <window.RunDetailPanel run={D.runsTable.find((r) => r.runId === openRun) || null} onClose={() => setOpenRun(null)} />
      <div style={{ marginTop: 12, display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 12 }}>
        <Button variant="outline" size="sm" disabled>Previous</Button>
        <span style={{ fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>Page 1</span>
        <Button variant="outline" size="sm">Next</Button>
      </div>
    </Panel>
  )
}

function CostDrivers() {
  const headers = [['Workflow'], ['Runs', true], ['Success', true], ['Cost', true], ['Cost / merged PR', true]]
  return (
    <Panel
      title="Top cost drivers"
      sub="By workflow"
      info="Where the money goes: total spend and efficiency per workflow in the selected period."
      stat={{ left: D.drivers.length + ' workflows', right: usd(D.drivers.reduce((s, d) => s + d.costUsd, 0)) + ' total' }}
    >
      <table style={{ width: '100%', borderCollapse: 'collapse' }}>
        <thead><tr>{headers.map(([h, r]) => <th key={h} style={TH(r)}>{h}</th>)}</tr></thead>
        <tbody>
          {D.drivers.map((d) => (
            <tr key={d.name}>
              <td style={{ ...TD(), fontWeight: 'var(--weight-medium)' }}>{d.name}</td>
              <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{d.runs}</td>
              <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{pct(d.successRate)}</td>
              <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{usd(d.costUsd)}</td>
              <td style={{ ...TD(true), fontFamily: 'var(--font-mono)' }}>{usd(d.costPerMergedPr)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}

/* "Cost by day" — the trailing-year calendar: 52 weeks across, 7 days down,
   Monday first. This density (364 squares) is the real test of the heatmap
   ramp; a 30-cell grid would not be. */
function CostByDayHeatmap() {
  const [selected, setSelected] = React.useState(null)
  const level = (cost) => (cost ? Math.min(5, Math.ceil((cost / D.maxHeatCost) * 5)) : 0)
  const label = selected && new Date(selected + 'T00:00:00Z').toLocaleString('en', { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' })
  const dayChip = label ? (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, borderRadius: 'var(--radius-full)', background: 'var(--muted)', padding: '2px 8px', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
      {label}
      <button type="button" aria-label={'Clear ' + label + ' filter'} onClick={() => setSelected(null)} style={{ border: 0, background: 'none', color: 'var(--foreground)', cursor: 'pointer', padding: '0 2px' }}>×</button>
    </span>
  ) : null

  return (
    <Panel
      title="Cost by day"
      info="Each square is a day; darker means more spend. Click a day to focus the whole page on it."
      extra={dayChip}
      stat={{ left: D.heatDays.length + ' days', right: 'peak ' + window.usdWholeAnalytics(D.maxHeatCost) + '/day' }}
    >
      <div style={{ display: 'flex', gap: 8 }}>
        <div style={{ display: 'flex', width: 24, flexDirection: 'column', textAlign: 'center', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
          <div style={{ height: 16, marginBottom: 4 }} aria-hidden="true" />
          <div style={{ display: 'grid', flex: 1, gridTemplateRows: 'repeat(7, 1fr)', gap: 3 }}>
            {['M', 'T', 'W', 'T', 'F', 'S', 'S'].map((d, i) => <span key={d + i} style={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>{d}</span>)}
          </div>
        </div>
        <div style={{ minWidth: 0, flex: 1 }}>
          <div style={{ position: 'relative', height: 16, marginBottom: 4, fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
            {D.monthLabels.map(({ week, label }) => <span key={week + label} style={{ position: 'absolute', left: (week / 52) * 100 + '%' }}>{label}</span>)}
          </div>
          <div style={{ display: 'grid', gridAutoFlow: 'column', gridTemplateColumns: 'repeat(52, minmax(0, 1fr))', gridTemplateRows: 'repeat(7, 1fr)', gap: 3 }}>
            {D.heatDays.map((d) => {
              const l = level(d.cost)
              const on = d.iso === selected
              return (
                <button
                  key={d.iso}
                  onClick={() => setSelected(on ? null : d.iso)}
                  title={new Date(d.iso + 'T00:00:00Z').toLocaleString('en', { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' }) + ' · ' + window.usdWholeAnalytics(d.cost) + ' · ' + d.runs + ' runs'}
                  style={{
                    aspectRatio: '1 / 1', cursor: 'pointer', borderRadius: 2,
                    border: '1px solid rgba(0,0,0,0.05)', padding: 0,
                    background: l ? 'var(--heatmap-' + l + ')' : 'var(--muted)',
                    outline: on ? '2px solid var(--primary)' : 'none', outlineOffset: 1,
                  }}
                />
              )
            })}
          </div>
        </div>
      </div>
      <div style={{ marginTop: 12, display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: 4, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
        Less
        {[1, 2, 3, 4, 5].map((i) => <i key={i} style={{ width: 12, height: 12, borderRadius: 2, background: 'var(--heatmap-' + i + ')' }} />)}
        More
      </div>
    </Panel>
  )
}

Object.assign(window, { TicketsTable, CostDrivers, CostByDayHeatmap, AnalyticsPanel: Panel })
