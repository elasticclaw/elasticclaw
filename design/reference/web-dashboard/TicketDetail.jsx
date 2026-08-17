/* Business drill-down for a unique ticket. Deliberately NOT the run panel:
   no spans, no tokens-per-attempt, no exit codes. It answers the questions a
   requester or a lead asks — what was asked for, how long it took from the
   report, what it cost in total, how much human time it pulled in, and where
   the delivery stands right now. The technical view stays one click away. */

const TDS = window.ElasticClawDesignSystem_0b1de0
const { Button: TButton, Icon: TIcon } = TDS
const tFormatUSD = window.formatRunUSD
const tFormatLabel = window.formatRunLabel

const TICKET_STATUS = {
  delivered: { icon: 'CheckCircle2', label: 'Delivered', color: 'var(--chart-2)', title: 'At least one pull request for this ticket was merged.' },
  pr_open: { icon: 'GitPullRequest', label: 'PR open', color: 'var(--chart-1)', title: 'Pull requests are open and awaiting review — none merged or closed yet.' },
  in_progress: { icon: 'CircleDot', label: 'In progress', color: 'var(--chart-5)', title: 'An agent is still working; no pull request has been opened yet.' },
  failed: { icon: 'XCircle', label: 'Failed', color: 'var(--chart-4)', title: 'Every run on this ticket failed and nothing was delivered.' },
}

function TicketStatusBadge({ status, size = 12 }) {
  const s = TICKET_STATUS[status] || TICKET_STATUS.in_progress
  return (
    <span
      title={s.title}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4, width: 'fit-content', whiteSpace: 'nowrap',
        padding: '2px 8px', border: '1px solid ' + 'color-mix(in srgb, ' + s.color + ' 45%, transparent)',
        borderRadius: 'var(--radius-md)', fontSize: 'var(--text-12)', fontWeight: 'var(--weight-medium)', color: s.color,
      }}
    >
      <TIcon name={s.icon} size={size} />
      {s.label}
    </span>
  )
}

const PR_STATE = {
  merged: { color: 'var(--chart-2)', icon: 'GitMerge', label: 'Merged' },
  open: { color: 'var(--chart-1)', icon: 'GitPullRequest', label: 'Open' },
  closed: { color: 'var(--muted-foreground)', icon: 'XCircle', label: 'Closed' },
}

const TCAPTION = {
  fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)',
  textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)',
}

function TKpi({ label, value, sub, tone }) {
  return (
    <div style={{ display: 'flex', minWidth: 0, flexDirection: 'column', gap: 3, border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)', padding: '10px 12px' }}>
      <span style={{ ...TCAPTION, color: 'var(--muted-foreground)' }}>{label}</span>
      <span style={{ fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-semibold)', fontVariantNumeric: 'tabular-nums', letterSpacing: 'var(--tracking-tight)', color: tone === 'error' ? 'var(--text-error)' : 'var(--foreground)' }}>{value}</span>
      {sub && <span style={{ fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{sub}</span>}
    </div>
  )
}

function TSection({ title, stat, children }) {
  return (
    <section style={{ display: 'flex', flexShrink: 0, flexDirection: 'column', overflow: 'hidden', border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, borderBottom: '1px solid var(--border)', padding: '8px 12px' }}>
        <h3 style={{ margin: 0, fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)' }}>{title}</h3>
        {stat && <span style={{ marginLeft: 'auto', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>{stat}</span>}
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6, padding: 12 }}>{children}</div>
    </section>
  )
}

function TRow({ label, value, mono = false }) {
  return (
    <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, padding: '3px 0' }}>
      <span style={{ ...TCAPTION, flexShrink: 0, width: 108, color: 'var(--muted-foreground)' }}>{label}</span>
      <span style={{ minWidth: 0, fontSize: 'var(--text-sm)', fontFamily: mono ? 'var(--font-mono)' : 'inherit', overflowWrap: 'anywhere' }}>{value}</span>
    </div>
  )
}

/* The ticket story: one line per business milestone, chronological, colored only
   where the milestone is genuinely good, bad, or human-driven. */
function StoryRow({ item, last }) {
  const color = item.kind === 'good' ? 'var(--chart-2)' : item.kind === 'bad' ? 'var(--chart-4)' : item.kind === 'human' ? 'var(--chart-1)' : 'var(--muted-foreground)'
  return (
    <div style={{ display: 'flex', gap: 10, minWidth: 0 }}>
      <div style={{ display: 'flex', flexShrink: 0, flexDirection: 'column', alignItems: 'center', width: 10 }}>
        <span style={{ marginTop: 5, width: 7, height: 7, flexShrink: 0, borderRadius: 'var(--radius-full)', background: color }} />
        {!last && <span style={{ flex: 1, width: 1, background: 'var(--border)' }} />}
      </div>
      <div style={{ minWidth: 0, flex: 1, paddingBottom: last ? 0 : 10 }}>
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, minWidth: 0 }}>
          <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-sm)', color: item.kind === 'neutral' ? 'var(--foreground)' : color }}>
            {item.label}{item.count > 1 ? ' (' + item.count + '×)' : ''}
          </span>
          <span style={{ marginLeft: 'auto', flexShrink: 0, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{item.time}</span>
        </div>
        <div style={{ fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{item.actor}</div>
      </div>
    </div>
  )
}

function TicketPrRow({ pr }) {
  const [hover, setHover] = React.useState(false)
  const s = PR_STATE[pr.state] || PR_STATE.open
  return (
    <a
      href={pr.url} target="_blank" rel="noreferrer"
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex', alignItems: 'center', gap: 8, textDecoration: 'none', color: 'var(--foreground)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', padding: '8px 12px',
        fontSize: 'var(--text-sm)', background: hover ? 'var(--accent)' : 'transparent',
      }}
    >
      <span style={{ display: 'inline-flex', color: s.color }}><TIcon name={s.icon} size={14} /></span>
      <span style={{ fontFamily: 'var(--font-mono)' }}>{pr.repo}#{pr.prNumber}</span>
      <span style={{ ...TCAPTION, color: s.color }}>{s.label}</span>
      <span style={{ marginLeft: 'auto', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{pr.runId}</span>
    </a>
  )
}

/* A run, seen from the business side: which attempt in the sequence, what it
   cost, what it produced. Clicking hands off to the technical panel. */
function TicketRunRow({ run, index, onOpen }) {
  const [hover, setHover] = React.useState(false)
  const produced = run.prs.length ? run.prs.map((p) => '#' + p.prNumber).join(', ') : run.status === 'running' ? 'still working' : 'nothing'
  return (
    <button
      type="button" onClick={() => onOpen(run)}
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex', alignItems: 'center', gap: 10, width: '100%', textAlign: 'left', cursor: 'pointer',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', padding: '8px 12px',
        background: hover ? 'var(--accent)' : 'transparent', color: 'var(--foreground)', fontSize: 'var(--text-sm)',
      }}
    >
      <span style={{ ...TCAPTION, flexShrink: 0, width: 40, color: 'var(--muted-foreground)' }}>Try {index + 1}</span>
      <window.RunStatusBadge status={run.status} />
      <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: 'var(--muted-foreground)' }}>
        produced {produced}
      </span>
      <span style={{ marginLeft: 'auto', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)' }}>
        <span>{run.duration}</span>
        <span>{tFormatUSD(run.cost)}</span>
        <span style={{ display: 'inline-flex', color: 'var(--muted-foreground)' }}><TIcon name="ChevronRight" size={14} /></span>
      </span>
    </button>
  )
}

function TicketDetailPanel({ ticket, onClose, onOpenRun }) {
  React.useEffect(() => {
    if (!ticket) return
    const esc = (e) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', esc)
    return () => document.removeEventListener('keydown', esc)
  }, [ticket, onClose])
  if (!ticket) return null

  const runs = ticket.runIds.map((id) => window.EC_ANALYTICS.runsTable.find((r) => r.runId === id))
  const open = ticket.prs.filter((p) => p.state === 'open')

  return ReactDOM.createPortal(
    <>
      <div onClick={onClose} aria-hidden="true" style={{ position: 'fixed', inset: 0, zIndex: 45, background: 'var(--overlay-scrim)' }} />
      <aside
        style={{
          position: 'fixed', inset: '0 0 0 auto', zIndex: 50, display: 'flex', flexDirection: 'column',
          width: '100%', maxWidth: '66vw', borderLeft: '1px solid var(--border)',
          background: 'var(--background)', boxShadow: 'var(--shadow-xl)',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, borderBottom: '1px solid var(--border)', background: 'var(--card)', padding: 16 }}>
          <div style={{ minWidth: 0, display: 'flex', flexDirection: 'column', gap: 5 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
              <TicketStatusBadge status={ticket.status} />
              <span style={{ ...TCAPTION, color: 'var(--muted-foreground)' }}>{ticket.priority} priority</span>
              <span style={{ ...TCAPTION, color: 'var(--muted-foreground)' }}>via {ticket.source}</span>
            </div>
            <h2 style={{ margin: 0, minWidth: 0, fontSize: 'var(--text-base)', fontWeight: 'var(--weight-semibold)', letterSpacing: 'var(--tracking-tight)', textWrap: 'pretty' }}>
              {ticket.issueId}: {ticket.issueTitle}
            </h2>
            <div style={{ minWidth: 0, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
              {ticket.requester} · {ticket.team} · reported {ticket.reportedAtLabel}
            </div>
            <div style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{ticket.repo} · {ticket.workflowName}</div>
          </div>
          <div style={{ display: 'flex', flexShrink: 0, alignItems: 'center', gap: 8 }}>
            {open.length > 0 && (
              <TButton variant="outline" size="sm" onClick={() => window.open(open[0].url, '_blank')}>
                <TIcon name="GitPullRequest" size={16} />Review PR
              </TButton>
            )}
            <TButton variant="ghost" size="icon-sm" onClick={onClose} title="Close ticket panel"><TIcon name="XCircle" size={16} /></TButton>
          </div>
        </div>

        <div style={{ minHeight: 0, flex: 1, overflowY: 'auto', padding: 16, display: 'flex', flexDirection: 'column', gap: 16 }}>
          {/* Delivery economics first — the four numbers a lead reports upward. */}
          <div style={{ display: 'grid', flexShrink: 0, gridTemplateColumns: 'repeat(auto-fit, minmax(132px, 1fr))', gap: 8 }}>
            <TKpi label="Lead time" value={ticket.leadTime || '—'} sub={ticket.deliveredAtLabel ? 'report → merge' : 'report → last activity'} />
            <TKpi label="Time to first run" value={ticket.timeToFirstRun || '—'} sub="report → agent started" />
            <TKpi label="Total cost" value={tFormatUSD(ticket.cost)} sub={ticket.runCount + ' runs · ' + ticket.attemptCount + ' attempts'} />
            <TKpi label="Human touches" value={ticket.humanTouches} sub={ticket.humanTouches === 0 ? 'fully autonomous' : 'reviews and nudges'} />
          </div>

          <TSection title="What was asked for">
            <p style={{ margin: 0, fontSize: 'var(--text-sm)', lineHeight: 'var(--leading-relaxed)', textWrap: 'pretty' }}>{ticket.ask}</p>
            <div style={{ marginTop: 4 }}>
              <TRow label="Requested by" value={ticket.requester + ' · ' + ticket.requesterRole} />
              <TRow label="Team" value={ticket.team} />
              <TRow label="Ticket" value={ticket.issueId} mono />
            </div>
          </TSection>

          <TSection title="Where it stands" stat={ticket.runCount + ' runs · ' + ticket.failedRunCount + ' failed'}>
            <p style={{ margin: 0, fontSize: 'var(--text-sm)', lineHeight: 'var(--leading-relaxed)', color: ticket.outcome ? 'var(--foreground)' : 'var(--muted-foreground)', textWrap: 'pretty' }}>
              {ticket.outcome || (ticket.status === 'pr_open'
                ? 'Work is done and waiting on a human: ' + ticket.openPrCount + ' pull request' + (ticket.openPrCount === 1 ? '' : 's') + ' open, none merged or closed yet.'
                : 'Still in flight — no pull request has been opened for this ticket yet.')}
            </p>
          </TSection>

          <TSection title="Delivery" stat={ticket.mergedPrCount + ' merged · ' + ticket.openPrCount + ' open'}>
            {ticket.prs.length === 0
              ? <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>No pull requests were opened for this ticket.</p>
              : ticket.prs.map((pr) => <TicketPrRow key={pr.id} pr={pr} />)}
          </TSection>

          <TSection title="How it went" stat={ticket.story.length + ' milestones'}>
            {ticket.story.map((item, i) => <StoryRow key={item.id} item={item} last={i === ticket.story.length - 1} />)}
          </TSection>

          <TSection title="Runs on this ticket" stat={tFormatUSD(ticket.cost) + ' total'}>
            {runs.map((run, i) => <TicketRunRow key={run.runId} run={run} index={i} onOpen={onOpenRun} />)}
            <p style={{ margin: '2px 0 0', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>Open a run for the technical view — spans, attempts, token usage.</p>
          </TSection>
        </div>
      </aside>
    </>,
    document.body,
  )
}

Object.assign(window, { TicketDetailPanel, TicketStatusBadge, TICKET_STATUS, TICKET_PR_STATE: PR_STATE })
