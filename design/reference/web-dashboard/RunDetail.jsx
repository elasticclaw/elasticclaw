const DS = window.ElasticClawDesignSystem_0b1de0
const { Button, Badge, Icon, Select, StepRow } = DS

/* Drill-down for the Runs table — web/components/analytics-command-center.tsx
   `RunDetailPanel` (scrim + right-hand sheet) plus `RunLogsDialog` (the Agent
   logs modal with Actions / Output tabs). Formatters mirror the source's Intl
   setup; `formatLabel` is its underscore→Title Case helper. */

const usdFmt = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 })
const tokenFmt = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 })
const formatUSD = (v) => (v == null ? '—' : usdFmt.format(v))
const formatTokens = (v) => (v == null ? '—' : tokenFmt.format(v))
const formatLabel = (v) => String(v || '').replace(/_/g, ' ').replace(/\b\w/g, (m) => m.toUpperCase())

const STATUS_ICON = { clean: 'CheckCircle2', human_in_the_loop: 'Users', failed: 'XCircle' }
const STATUS_TITLE = {
  clean: 'PR merged or closed with zero human interaction.',
  human_in_the_loop: 'PR merged or closed; a human interacted via the PR (comment, review, or code push).',
  warning: 'PR merged or closed; a human interacted via the factory dashboard.',
  failed: 'No PR was ever delivered or the run definitively failed before delivery.',
  running: 'In progress; no failure has occurred.',
}
const STATUS_STYLE = {
  clean: { color: 'var(--chart-2)', borderColor: 'color-mix(in srgb, var(--chart-2) 40%, transparent)' },
  human_in_the_loop: { color: 'var(--chart-1)', borderColor: 'color-mix(in srgb, var(--chart-1) 50%, transparent)' },
  warning: { color: 'var(--chart-3)', borderColor: 'color-mix(in srgb, var(--chart-3) 50%, transparent)' },
  failed: { background: 'var(--destructive)', color: '#fff', borderColor: 'transparent' },
  running: { background: 'var(--secondary)', color: 'var(--secondary-foreground)', borderColor: 'transparent' },
}

function RunStatusBadge({ status }) {
  return (
    <span
      title={STATUS_TITLE[status]}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4, width: 'fit-content', whiteSpace: 'nowrap',
        padding: '2px 8px', border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
        fontSize: 'var(--text-12)', fontWeight: 'var(--weight-medium)', ...STATUS_STYLE[status],
      }}
    >
      <Icon name={STATUS_ICON[status] || 'CircleDot'} size={12} />
      {formatLabel(status)}
    </span>
  )
}

/* The panel reuses the analytics screen's two patterns: the KPI tile (uppercase
   micro caption + monospaced value) for the run's headline numbers, and the card
   anatomy (hairline header, body, monospaced stat line) for every section. Flat
   grids of equally-weighted bordered boxes are gone — they gave a duration the
   same weight as a phase name. */

const CAPTION = {
  fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)',
  textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)',
  color: 'var(--muted-foreground)',
}

/* `tone="error"` is only for values that genuinely denote failure. */
function KpiTile({ label, value, sub, tone }) {
  return (
    <div style={{ display: 'flex', minWidth: 0, flexDirection: 'column', gap: 3, border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)', padding: '10px 12px' }}>
      <span style={CAPTION}>{label}</span>
      <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-medium)', letterSpacing: 'var(--tracking-tight)', color: tone === 'error' ? 'var(--text-error)' : 'var(--foreground)' }}>{value}</span>
      {sub && <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-2xs)', color: 'var(--muted-foreground)' }}>{sub}</span>}
    </div>
  )
}

function Section({ title, stat, children }) {
  return (
    <section style={{ display: 'flex', flexShrink: 0, flexDirection: 'column', overflow: 'hidden', border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, borderBottom: '1px solid var(--border)', padding: '8px 12px' }}>
        <h3 style={{ margin: 0, flex: 1, minWidth: 0, fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)' }}>{title}</h3>
      </div>
      <div style={{ padding: 12 }}>{children}</div>
      {stat && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, borderTop: '1px solid var(--border)', padding: '4px 12px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
          <span>{stat.left}</span>
          {stat.right && <span style={{ marginLeft: 'auto', color: stat.tone === 'error' ? 'var(--text-error)' : undefined }}>{stat.right}</span>}
        </div>
      )}
    </section>
  )
}

/* Definition row: caption left at a fixed measure, value right in mono when it
   is a machine string. Cheaper vertically than a bordered box per field. */
function Row({ label, value, mono = true, tone }) {
  return (
    <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, padding: '3px 0' }}>
      <span style={{ ...CAPTION, width: 104, flexShrink: 0 }}>{label}</span>
      <span style={{ minWidth: 0, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontFamily: mono ? 'var(--font-mono)' : 'var(--font-sans)', fontSize: 'var(--text-12)', color: tone === 'error' ? 'var(--text-error)' : tone === 'muted' ? 'var(--muted-foreground)' : 'var(--foreground)' }}>{value}</span>
    </div>
  )
}

/* Timing rows carry a proportional bar — the delivery funnel's treatment, so a
   phase that dominates the run is visible without reading the numbers. */
function TimingRow({ label, value, share, title }) {
  return (
    <div title={title} style={{ padding: '4px 0' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, marginBottom: 3, fontSize: 'var(--text-12)' }}>
        <span>{label}</span>
        <span style={{ fontFamily: 'var(--font-mono)', color: 'var(--muted-foreground)' }}>{value}</span>
      </div>
      <div style={{ height: 6, borderRadius: 'var(--radius-full)', background: 'var(--muted)' }}>
        <div style={{ height: '100%', width: Math.max(2, share * 100) + '%', borderRadius: 'var(--radius-full)', background: 'var(--chart-1)' }} />
      </div>
    </div>
  )
}

const ROW = { border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', padding: '8px 12px', fontSize: 'var(--text-sm)' }

/* Minutes from "1h 18m" / "38m" / "1m 12s" / "0s", for the timing bars. */
function toMinutes(v) {
  const h = /(\d+(?:\.\d+)?)h/.exec(v)
  const m = /(\d+(?:\.\d+)?)m(?!s)/.exec(v)
  const s = /(\d+(?:\.\d+)?)s/.exec(v)
  return (h ? +h[1] * 60 : 0) + (m ? +m[1] : 0) + (s ? +s[1] / 60 : 0)
}

/* Agent logs modal: Actions (the agent's tool activity) and Output (pipeline
   stdout/stderr per stage). The source's Actions tab embeds ClawActivityLog;
   here the same activity renders through the design system's StepRow. */
function AgentLogsDialog({ run, open, onClose }) {
  // A run that never reached the agent (failed provisioning) has no activity —
  // open on Output rather than on an empty tab.
  const [tab, setTab] = React.useState(run.activity.length === 0 && run.outputs.length > 0 ? 'output' : 'actions')
  const [attempt, setAttempt] = React.useState('')
  if (!open) return null
  return ReactDOM.createPortal(
    <>
      <div onClick={onClose} aria-hidden="true" style={{ position: 'fixed', inset: 0, zIndex: 70, background: 'var(--overlay-scrim)' }} />
      <div
        role="dialog" aria-label="Agent logs"
        style={{
          position: 'fixed', zIndex: 71, top: '50%', left: '50%', transform: 'translate(-50%, -50%)',
          display: 'flex', flexDirection: 'column', width: 'min(1024px, 92vw)', height: 'min(85vh, 800px)',
          overflow: 'hidden', border: '1px solid var(--border)', borderRadius: 'var(--radius-xl)',
          background: 'var(--card)', boxShadow: 'var(--shadow-xl)',
        }}
      >
        <div style={{ flexShrink: 0, display: 'flex', alignItems: 'flex-start', gap: 12, borderBottom: '1px solid var(--border)', padding: '20px 24px' }}>
          <div style={{ minWidth: 0, flex: 1 }}>
            <div style={{ fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-semibold)' }}>Agent logs</div>
            <div style={{ marginTop: 2, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
              Agent activity and pipeline output for {run.ownerDisplayName || run.runId}.
            </div>
          </div>
          <Button variant="ghost" size="icon-sm" onClick={onClose} title="Close"><Icon name="X" size={16} /></Button>
        </div>

        <div style={{ flexShrink: 0, display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, padding: '12px 24px' }}>
          <div style={{ display: 'flex', gap: 2, padding: 2, border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', background: 'var(--surface-subtle)' }}>
            {[['actions', 'Actions'], ['output', 'Output']].map(([id, label]) => (
              <button
                key={id} type="button" onClick={() => setTab(id)}
                style={{
                  padding: '3px 10px', border: 0, borderRadius: 'var(--radius-sm)', cursor: 'pointer',
                  fontFamily: 'var(--font-sans)', fontSize: 'var(--text-12)',
                  background: tab === id ? 'var(--secondary)' : 'transparent',
                  color: tab === id ? 'var(--foreground)' : 'var(--muted-foreground)',
                  fontWeight: tab === id ? 'var(--weight-medium)' : 'var(--weight-normal)',
                }}
              >{label}</button>
            ))}
          </div>
          {tab === 'actions' && run.attempts.length > 1 && (
            <div style={{ width: 176 }}>
              <Select size="sm" value={attempt} onChange={setAttempt} placeholder="Select attempt" options={run.attempts.map((a) => 'Attempt ' + a.attemptNumber)} />
            </div>
          )}
        </div>

        <div style={{ minHeight: 0, flex: 1, overflowY: 'auto', padding: '0 24px 24px' }}>
          {tab === 'actions' ? (
            run.activity.length === 0
              ? <Empty>No agent activity was recorded for this run.</Empty>
              : <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>{run.activity.map((s, i) => <StepRow key={i} {...s} />)}</div>
          ) : (
            <OutputTab run={run} />
          )}
        </div>
      </div>
    </>,
    document.body
  )
}

function Empty({ children }) {
  return <p style={{ padding: '32px 0', textAlign: 'center', margin: 0, fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>{children}</p>
}

/* ---------------------------------------------------------------------------
   Pipeline output, read as OpenTelemetry.

   The upstream Output tab prints raw stdout/stderr blobs per stage, which is
   unreadable once a stage emits more than a few lines. Same data, OTEL shape:
   a resource header (service.name, trace_id), one SPAN per stage output
   (span_id, kind, duration, status OK/ERROR), and LOG RECORDS carrying a
   timestamp, a severity, a body and typed attributes. Severity filter narrows
   the stream; "Raw" falls back to the verbatim stdout/stderr the source shows.
   --------------------------------------------------------------------------- */

const SEVERITY = {
  TRACE: { rank: 1, color: 'var(--muted-foreground)', bg: 'var(--muted)' },
  DEBUG: { rank: 5, color: 'var(--muted-foreground)', bg: 'var(--muted)' },
  INFO: { rank: 9, color: 'var(--chart-1)', bg: 'color-mix(in srgb, var(--chart-1) 15%, transparent)' },
  WARN: { rank: 13, color: 'var(--chart-3)', bg: 'color-mix(in srgb, var(--chart-3) 15%, transparent)' },
  ERROR: { rank: 17, color: 'var(--chart-4)', bg: 'color-mix(in srgb, var(--chart-4) 15%, transparent)' },
  FATAL: { rank: 21, color: '#fff', bg: 'var(--destructive)' },
}

function SeverityChip({ sev }) {
  const s = SEVERITY[sev] || SEVERITY.INFO
  return (
    <span
      title={'severity_text=' + sev + ' · severity_number=' + s.rank}
      style={{
        display: 'inline-flex', alignItems: 'center', justifyContent: 'center', width: 46, flexShrink: 0,
        padding: '1px 0', borderRadius: 'var(--radius-sm)', background: s.bg, color: s.color,
        fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)',
        letterSpacing: '0.03em',
      }}
    >{sev}</span>
  )
}

function AttrChip({ k, v }) {
  const numeric = typeof v === 'number'
  return (
    <span style={{ display: 'inline-flex', alignItems: 'baseline', gap: 3, padding: '0 4px', borderRadius: 'var(--radius-sm)', background: 'var(--muted)', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)' }}>
      <span style={{ color: 'var(--muted-foreground)' }}>{k}</span>
      <span style={{ color: 'var(--muted-foreground)' }}>=</span>
      <span style={{ color: numeric ? 'var(--chart-3)' : 'var(--foreground)' }}>{String(v)}</span>
    </span>
  )
}

function LogRecord({ rec }) {
  const [hover, setHover] = React.useState(false)
  const s = SEVERITY[rec.sev] || SEVERITY.INFO
  const bad = rec.sev === 'ERROR' || rec.sev === 'FATAL'
  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'grid', gridTemplateColumns: 'auto auto 1fr', alignItems: 'baseline', columnGap: 10,
        padding: '4px 12px', borderLeft: '2px solid ' + (bad ? s.color : 'transparent'),
        background: hover ? 'var(--surface-subtle)' : 'transparent',
      }}
    >
      <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>{rec.t}</span>
      <SeverityChip sev={rec.sev} />
      <span style={{ minWidth: 0, display: 'flex', flexWrap: 'wrap', alignItems: 'baseline', gap: 6 }}>
        <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: bad ? s.color : 'var(--foreground)', overflowWrap: 'anywhere' }}>{rec.body}</span>
        {Object.entries(rec.attrs || {}).map(([k, v]) => <AttrChip key={k} k={k} v={v} />)}
      </span>
    </div>
  )
}

/* One span per stage output: name, ids, duration, status — the header a trace
   viewer would show above its log records. */
function SpanBlock({ output, minSev, raw }) {
  const ok = output.status !== 'ERROR'
  const records = (output.records || []).filter((r) => (SEVERITY[r.sev]?.rank ?? 9) >= minSev)
  const counts = (output.records || []).reduce((acc, r) => ({ ...acc, [r.sev]: (acc[r.sev] || 0) + 1 }), {})
  return (
    <section style={{ border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)', overflow: 'hidden' }}>
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8, borderBottom: '1px solid var(--border)', background: 'var(--surface-subtle)', padding: '8px 12px' }}>
        <Icon name="Braces" size={14} style={{ color: 'var(--muted-foreground)' }} />
        <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)' }}>{output.outputName}</span>
        <span style={{
          padding: '1px 6px', borderRadius: 'var(--radius-sm)', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)',
          background: ok ? 'color-mix(in srgb, var(--chart-2) 15%, transparent)' : 'color-mix(in srgb, var(--chart-4) 15%, transparent)',
          color: ok ? 'var(--chart-2)' : 'var(--chart-4)',
        }}>{output.status}</span>
        <span style={{ marginLeft: 'auto', display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
          <span title="span.kind">{output.spanKind}</span>
          <span title="span_id">span_id={output.spanId}</span>
          <span title="span duration">{(output.durationMs / 1000).toFixed(2)}s</span>
          <span title="process.exit_code">exit={output.exitCode}</span>
        </span>
      </div>

      {raw ? (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12, padding: 12 }}>
          {output.stdout && <LogStream label="stdout" value={output.stdout} />}
          {output.stderr && <LogStream label="stderr" value={output.stderr} error />}
          {!output.stdout && !output.stderr && <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>This output did not write to stdout or stderr.</p>}
        </div>
      ) : records.length === 0 ? (
        <p style={{ margin: 0, padding: '16px 12px', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>No records at this severity.</p>
      ) : (
        <div style={{ padding: '4px 0' }}>{records.map((r, i) => <LogRecord key={i} rec={r} />)}</div>
      )}

      <div style={{ display: 'flex', alignItems: 'center', gap: 10, borderTop: '1px solid var(--border)', padding: '4px 12px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
        <span>{(output.records || []).length} records</span>
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          {['WARN', 'ERROR', 'FATAL'].filter((s) => counts[s]).map((s) => (
            <span key={s} style={{ color: SEVERITY[s].color === '#fff' ? 'var(--text-error)' : SEVERITY[s].color }}>{counts[s]} {s.toLowerCase()}</span>
          ))}
          <span>{output.attemptId}</span>
        </span>
      </div>
    </section>
  )
}

const SEV_FILTERS = [['ALL', 0], ['DEBUG', 5], ['INFO', 9], ['WARN', 13], ['ERROR', 17]]

function OutputTab({ run }) {
  const [minSev, setMinSev] = React.useState(0)
  const [raw, setRaw] = React.useState(false)
  if (run.outputs.length === 0) return <Empty>No pipeline output was recorded for this run.</Empty>
  const stages = [...new Set(run.outputs.map((o) => o.stage))]
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      {/* Resource + trace context, once for the whole stream. */}
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8, border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)', padding: '8px 12px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
        <AttrChip k="service.name" v="elasticclaw-pipeline" />
        <AttrChip k="deployment.environment" v={run.workspaceName} />
        <AttrChip k="trace_id" v={run.traceId} />
        <span style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 8 }}>
          {SEV_FILTERS.map(([label, rank]) => (
            <button
              key={label} type="button" onClick={() => setMinSev(rank)}
              title={rank ? 'severity_number >= ' + rank : 'all severities'}
              style={{
                padding: '1px 6px', border: 0, borderRadius: 'var(--radius-sm)', cursor: 'pointer',
                fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)',
                background: minSev === rank ? 'var(--secondary)' : 'transparent',
                color: minSev === rank ? 'var(--foreground)' : 'var(--muted-foreground)',
              }}
            >{label}</button>
          ))}
          <span style={{ width: 1, alignSelf: 'stretch', background: 'var(--border)' }} />
          <button
            type="button" onClick={() => setRaw(!raw)}
            title="Verbatim stdout / stderr"
            style={{
              padding: '1px 6px', border: 0, borderRadius: 'var(--radius-sm)', cursor: 'pointer',
              fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)',
              background: raw ? 'var(--secondary)' : 'transparent',
              color: raw ? 'var(--foreground)' : 'var(--muted-foreground)',
            }}
          >RAW</button>
        </span>
      </div>

      {stages.map((stage) => (
        <div key={stage} style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span style={{ ...CAPTION }}>Stage</span>
            <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)' }}>{stage}</span>
            <span style={{ flex: 1, height: 1, background: 'var(--border)' }} />
          </div>
          {run.outputs.filter((o) => o.stage === stage).map((o) => (
            <SpanBlock key={o.outputName} output={o} minSev={minSev} raw={raw} />
          ))}
        </div>
      ))}
    </div>
  )
}

function LogStream({ label, value, error }) {
  return (
    <div>
      <div style={{ marginBottom: 4, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)', color: error ? 'var(--text-error)' : 'var(--muted-foreground)' }}>{label}</div>
      <pre style={{ margin: 0, maxHeight: 256, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word', borderRadius: 'var(--radius-md)', background: 'var(--muted)', padding: 12, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)' }}>{value}</pre>
    </div>
  )
}

/* Right-hand sheet: scrim + max-w-[66vw] panel, header with owner + run id,
   then Agent logs, the detail grid, Usage, Timing, PRs, Attempts, Events. */
function RunDetailPanel({ run, onClose }) {
  const [logs, setLogs] = React.useState(false)
  React.useEffect(() => {
    if (!run) return
    const esc = (e) => { if (e.key === 'Escape') { setLogs(false); onClose() } }
    document.addEventListener('keydown', esc)
    return () => document.removeEventListener('keydown', esc)
  }, [run, onClose])
  if (!run) return null

  const timingMax = Math.max(...run.timing.map((p) => toMinutes(p.value)), 0.001)
  const failedAttempts = run.attempts.filter((a) => a.status === 'failed').length

  return ReactDOM.createPortal(
    <>
      <div onClick={onClose} aria-hidden="true" style={{ position: 'fixed', inset: 0, zIndex: 55, background: 'var(--overlay-scrim)' }} />
      <aside
        style={{
          position: 'fixed', inset: '0 0 0 auto', zIndex: 60, display: 'flex', flexDirection: 'column',
          width: '100%', maxWidth: '66vw', borderLeft: '1px solid var(--border)',
          background: 'var(--background)', boxShadow: 'var(--shadow-xl)',
        }}
      >
        {/* Header carries the identity in three descending weights: what the run
            was for, who ran it, and the opaque id — plus the state as a badge,
            so Status/Phase/Failure are not three cells in a flat grid. */}
        <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, borderBottom: '1px solid var(--border)', background: 'var(--card)', padding: 16 }}>
          <div style={{ minWidth: 0, display: 'flex', flexDirection: 'column', gap: 5 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
              <RunStatusBadge status={run.status} />
              <span style={{ ...CAPTION, color: 'var(--muted-foreground)' }}>{formatLabel(run.phase)}</span>
              {run.failureType && <span style={{ ...CAPTION, color: 'var(--text-error)' }}>{formatLabel(run.failureType)}</span>}
              {run.warningTypes.map((w) => <span key={w} style={{ ...CAPTION, color: 'var(--text-warning)' }}>{formatLabel(w)}</span>)}
            </div>
            <h2 style={{ margin: 0, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-base)', fontWeight: 'var(--weight-semibold)', letterSpacing: 'var(--tracking-tight)' }}>
              {run.issueId ? run.issueId + ': ' + run.issueTitle : run.ownerDisplayName}
            </h2>
            <div style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
              {run.ownerDisplayName} · {run.workflowName} · {run.workspaceName}
            </div>
            <div style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{run.runId}</div>
          </div>
          <div style={{ display: 'flex', flexShrink: 0, alignItems: 'center', gap: 8 }}>
            <Button variant="outline" size="sm" onClick={() => setLogs(true)}><Icon name="FileTerminal" size={16} />Agent logs</Button>
            <Button variant="ghost" size="icon-sm" onClick={onClose} title="Close detail panel"><Icon name="XCircle" size={16} /></Button>
          </div>
        </div>

        <div style={{ minHeight: 0, flex: 1, overflowY: 'auto', padding: 16, display: 'flex', flexDirection: 'column', gap: 16 }}>
          {/* Headline numbers first, at KPI weight. */}
          <div style={{ display: 'grid', flexShrink: 0, gridTemplateColumns: 'repeat(auto-fit, minmax(132px, 1fr))', gap: 8 }}>
            <KpiTile label="Duration" value={run.duration} sub={run.startedAt} />
            <KpiTile label="Cost" value={formatUSD(run.cost)} sub={run.model} />
            <KpiTile label="Tokens" value={formatTokens(run.totalTokens)} sub={formatTokens(run.inputTokens) + ' in · ' + formatTokens(run.outputTokens) + ' out'} />
            {/* No tone: human-on-the-loop is a delivered outcome, not a failure.
                The source leaves this metric untoned too — it passes tone only for
                Clean / Warning / Failed. Red here would contradict the blue
                "Human In The Loop" badge in this same header. */}
            <KpiTile
              label="Human touches"
              value={String(run.humanInteractionCount)}
              sub={run.mergedPrCount + ' of ' + run.prCount + ' PRs merged'}
            />
          </div>

          <Section title="Run" stat={{ left: run.factoryName + ' · ' + run.workspaceName, right: run.runId }}>
            <Row label="Issue" value={run.issueId ? run.issueId + ': ' + run.issueTitle : 'None'} mono={false} />
            <Row label="Repo" value={run.repo || 'None'} />
            <Row label="Model" value={run.model || 'Unknown'} />
            <Row label="Workflow" value={run.workflowName || 'None'} />
            <Row label="Started" value={run.startedAt} />
            <Row label="Failure" value={run.failureType ? formatLabel(run.failureType) : 'None'} tone={run.failureType ? 'error' : 'muted'} mono={!!run.failureType} />
          </Section>

          {run.timing.length > 0 && (
            <Section title="Timing" stat={{ left: run.timing.length + ' phases', right: 'total ' + run.duration }}>
              {run.timing.map((p) => (
                <TimingRow key={p.label} label={p.label} value={p.value} title={p.title} share={toMinutes(p.value) / timingMax} />
              ))}
            </Section>
          )}

          <Section title="Pull requests" stat={{ left: run.prCount + (run.prCount === 1 ? ' PR' : ' PRs'), right: run.mergedPrCount + ' merged' }}>
            {run.prs.length === 0
              ? <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>No PRs recorded.</p>
              : <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>{run.prs.map((pr) => <PrRow key={pr.id} pr={pr} />)}</div>}
          </Section>

          <Section
            title="Attempts"
            stat={{ left: run.attempts.length + (run.attempts.length === 1 ? ' attempt' : ' attempts'), right: failedAttempts > 0 ? failedAttempts + ' failed' : undefined, tone: 'error' }}
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {run.attempts.map((a) => (
                <div key={a.id} style={ROW}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <span style={{ flex: 1, minWidth: 0 }}>Attempt {a.attemptNumber}</span>
                    <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{a.duration}</span>
                    <Badge variant="outline" tone={a.status === 'failed' ? 'error' : a.status === 'running' ? 'running' : undefined}>{a.status}</Badge>
                  </div>
                  {a.failureType && <div style={{ marginTop: 4, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: 'var(--text-error)' }}>{formatLabel(a.failureType)}</div>}
                </div>
              ))}
            </div>
          </Section>

          {/* Events read as a timeline: a rail with a dot per event, newest first. */}
          <Section title="Events" stat={{ left: run.events.length + ' events', right: 'newest first' }}>
            <div style={{ display: 'flex', flexDirection: 'column' }}>
              {[...run.events].reverse().slice(0, 12).map((e, i, all) => (
                <div key={e.id} style={{ display: 'flex', gap: 10, minWidth: 0 }}>
                  <span style={{ position: 'relative', display: 'flex', width: 8, flexShrink: 0, justifyContent: 'center' }}>
                    <span style={{ position: 'absolute', top: 12, bottom: 0, width: 1, background: i === all.length - 1 ? 'transparent' : 'var(--border)' }} />
                    <span style={{ position: 'relative', marginTop: 7, width: 6, height: 6, borderRadius: 'var(--radius-full)', background: i === 0 ? 'var(--chart-1)' : 'var(--muted-foreground)' }} />
                  </span>
                  <div style={{ minWidth: 0, flex: 1, paddingBottom: i === all.length - 1 ? 0 : 10 }}>
                    <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
                      <span style={{ minWidth: 0, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontSize: 'var(--text-12)' }}>{formatLabel(e.eventType)}</span>
                      <span style={{ flexShrink: 0, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{e.time}</span>
                    </div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>{e.actor} · {e.source}</div>
                  </div>
                </div>
              ))}
            </div>
          </Section>
        </div>
      </aside>
      <AgentLogsDialog run={run} open={logs} onClose={() => setLogs(false)} />
    </>,
    document.body
  )
}

function PrRow({ pr }) {
  const [hover, setHover] = React.useState(false)
  return (
    <a
      href={pr.url} target="_blank" rel="noreferrer"
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8,
        border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', padding: '8px 12px',
        fontSize: 'var(--text-sm)', color: 'var(--foreground)', textDecoration: 'none',
        background: hover ? 'var(--accent)' : 'transparent',
      }}
    >
      <span style={{ display: 'flex', minWidth: 0, alignItems: 'center', gap: 8 }}>
        <Icon name="GitPullRequest" size={16} style={{ color: 'var(--muted-foreground)' }} />
        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontFamily: 'var(--font-mono)' }}>{pr.repo}#{pr.prNumber}</span>
      </span>
      <Icon name="ExternalLink" size={12} style={{ color: 'var(--muted-foreground)' }} />
    </a>
  )
}

Object.assign(window, { RunDetailPanel, RunStatusBadge, AgentLogsDialog, formatRunLabel: formatLabel, formatRunUSD: formatUSD })
