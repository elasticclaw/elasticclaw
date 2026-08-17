const DS = window.ElasticClawDesignSystem_0b1de0
const { Button, Icon, StatusDot, ContextProgressBar, BootstrapProgress, Tag, NowStrip, StepRow, MessageBubble, Composer } = DS
const { AGENT_SECTION, agentSection } = window

/* Header actions. Icon-only buttons were unlabelled guesses, so each one now
   states itself: an aria-label, a title, and — for the two that carry a payload —
   a count beside the glyph. Copy confirms in place instead of silently working. */
function CardAction({ icon, label, count, onClick, tone }) {
  const [hover, setHover] = React.useState(false)
  return (
    <button
      type="button" onClick={onClick} aria-label={label} title={label}
      onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4, flexShrink: 0, cursor: 'pointer',
        height: 24, padding: count == null ? '0 5px' : '0 7px 0 5px',
        border: '1px solid ' + (count == null ? 'transparent' : 'var(--border)'),
        borderRadius: 'var(--radius-md)',
        background: hover ? 'var(--accent)' : 'transparent',
        color: tone || (hover ? 'var(--foreground)' : 'var(--muted-foreground)'),
        fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', fontWeight: 'var(--weight-medium)',
      }}
    >
      <Icon name={icon} size={14} />
      {count != null && <span>{count}</span>}
    </button>
  )
}

/* Quick access to the agent's open PRs: the count is on the button, the list is
   one click away, and a single PR still lists rather than jumping — the card
   should never navigate somewhere the user cannot see first. */
function OpenPrsButton({ prs }) {
  const [open, setOpen] = React.useState(false)
  React.useEffect(() => {
    if (!open) return
    const away = () => setOpen(false)
    document.addEventListener('click', away)
    return () => document.removeEventListener('click', away)
  }, [open])
  if (!prs || prs.length === 0) return null
  return (
    <span style={{ position: 'relative', display: 'inline-flex' }} onClick={(e) => e.stopPropagation()}>
      <CardAction
        icon="GitPullRequest" count={prs.length}
        label={prs.length + ' open pull request' + (prs.length === 1 ? '' : 's')}
        tone="var(--chart-1)"
        onClick={() => setOpen((v) => !v)}
      />
      {open && (
        <div style={{ position: 'absolute', top: 28, right: 0, zIndex: 20, width: 260, display: 'flex', flexDirection: 'column', gap: 2, border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--popover)', boxShadow: 'var(--shadow-lg)', padding: 6 }}>
          <span style={{ padding: '2px 6px', fontSize: 'var(--text-2xs)', fontWeight: 'var(--weight-medium)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)', color: 'var(--muted-foreground)' }}>Open pull requests</span>
          {prs.map((pr) => (
            <a
              key={pr.prNumber} href={pr.url} target="_blank" rel="noreferrer"
              style={{ display: 'flex', flexDirection: 'column', gap: 1, textDecoration: 'none', borderRadius: 'var(--radius-md)', padding: '5px 6px', color: 'var(--foreground)' }}
              onMouseEnter={(e) => { e.currentTarget.style.background = 'var(--accent)' }}
              onMouseLeave={(e) => { e.currentTarget.style.background = 'transparent' }}
            >
              <span style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: 'var(--font-mono)', fontSize: 'var(--text-12)', color: 'var(--chart-1)' }}>
                <Icon name="GitPullRequest" size={12} />{pr.repo + '#' + pr.prNumber}
              </span>
              <span style={{ fontSize: 'var(--text-12)', color: 'var(--muted-foreground)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{pr.title}</span>
            </a>
          ))}
        </div>
      )}
    </span>
  )
}

function BoardCard({ agent, onOpen, onSend }) {
  const [copied, setCopied] = React.useState(false)
  const pending = agent.status === 'provisioning' || agent.status === 'error' || agent.status === 'offline'
  const section = agentSection(agent)
  const sectionMeta = AGENT_SECTION[section]
  // Every card carries its group's rail, not just the streaming ones — the board
  // has to be readable without counting dots.
  const rail = agent.isStreaming ? 'var(--status-streaming)' : agent.status === 'provisioning' ? 'var(--status-provisioning)' : agent.status === 'error' ? 'var(--status-error)' : sectionMeta.color
  return (
    <div style={{ position: 'relative', width: 'var(--board-card-width)', boxSizing: 'border-box', flexShrink: 0, height: '100%', display: 'flex', flexDirection: 'column', border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', background: 'var(--card)', opacity: pending ? 0.85 : 1, overflow: 'hidden' }}>
      {rail && <div style={{ position: 'absolute', left: 0, top: 0, bottom: 0, width: 4, background: rail, animation: agent.status === 'provisioning' ? 'ec-pulse 2s var(--ease-default) infinite' : undefined }} />}
      <div style={{ padding: '8px 12px 0' }}><ContextProgressBar usage={agent.contextUsage} /></div>
      <div style={{ padding: 12, borderBottom: '1px solid var(--border)' }}>
        <div style={{ display: 'flex', alignItems: 'flex-start', gap: 8 }}>
          <span style={{ color: 'var(--muted-foreground)', display: 'flex', marginTop: 2, cursor: 'grab' }} title="Drag to reorder"><Icon name="GripVertical" size={14} /></span>
          {/* Identity block: name, then what it came from and how long it has been
              up — read as one unit instead of competing with the action icons. */}
          <div style={{ minWidth: 0, flex: 1, display: 'flex', flexDirection: 'column', gap: 2 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
              <StatusDot status={agent.status} isStreaming={agent.isStreaming} />
              <button onClick={() => !pending && onOpen(agent.id)} title={pending ? undefined : 'Open conversation'} style={{ minWidth: 0, textAlign: 'left', border: 0, background: 'none', padding: 0, cursor: pending ? 'default' : 'pointer', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)', color: 'var(--foreground)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{agent.name}</button>
              {agent.unreadCount > 0 && <span title={agent.unreadCount + ' unread replies'} style={{ flexShrink: 0, padding: '1px 6px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', fontWeight: 'var(--weight-medium)', background: 'var(--blue-500)', color: '#fff', borderRadius: 'var(--radius-full)' }}>{agent.unreadCount}</span>}
            </div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>
              <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{agent.template}</span>
              <span aria-hidden="true">·</span>
              <span style={{ flexShrink: 0, fontFamily: 'var(--font-mono)', color: agent.status === 'provisioning' ? 'var(--status-provisioning)' : agent.status === 'error' ? 'var(--status-error)' : 'var(--muted-foreground)' }}>
                {agent.status === 'provisioning' ? 'starting...' : agent.status === 'error' ? 'error' : agent.uptime}
              </span>
            </div>
          </div>
          {/* Actions: PRs first (the one with a destination), then transcript, then
              details. Hairline-separated from the identity block. */}
          <div style={{ display: 'flex', flexShrink: 0, alignItems: 'center', gap: 2, paddingLeft: 8, borderLeft: '1px solid var(--border)' }}>
            <OpenPrsButton prs={agent.openPrs} />
            <CardAction
              icon={copied ? 'CheckCircle2' : 'ClipboardCopy'}
              label={copied ? 'Transcript copied' : 'Copy transcript'}
              tone={copied ? 'var(--chart-2)' : undefined}
              onClick={() => { setCopied(true); setTimeout(() => setCopied(false), 1400) }}
            />
            <CardAction icon="Info" label="Agent details" />
          </div>
        </div>
        {agent.status === 'provisioning' && <BootstrapProgress step={agent.bootstrapStep} />}
        {agent.tags.length > 0 && (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 10 }}>
            {agent.tags.slice(0, 3).map((t) => <Tag key={t}>{t}</Tag>)}
          </div>
        )}
      </div>
      {agent.now && <NowStrip variant="card" {...agent.now} />}
      <div style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: 12, display: 'flex', flexDirection: 'column', gap: 8 }}>
        {agent.messages.length === 0 && agent.steps.length === 0 && (
          <p style={{ textAlign: 'center', padding: '16px 0', margin: 0, fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>No messages yet</p>
        )}
        {agent.messages.map((m) => (
          <window.Bubble key={m.id} role={m.role} density="card" time={m.time} accent={agent.accent} {...window.messageAuthor(m, agent)}>{m.content}</window.Bubble>
        ))}
        {agent.steps.map((s, i) => <StepRow key={i} {...s} density="card" />)}
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, borderTop: '1px solid var(--border)', padding: '4px 12px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', color: 'var(--muted-foreground)' }}>
        <span>{agent.steps.length} step{agent.steps.length === 1 ? '' : 's'}</span>
        {agent.steps.some((s) => s.status === 'failed') && <span style={{ color: 'var(--text-error)' }}>1 failed</span>}
        <span style={{ marginLeft: 'auto' }}>ctx {agent.contextUsage}%</span>
      </div>
      <Composer
        density="card"
        disabled={pending}
        placeholder={pending ? (agent.status === 'error' ? 'Provisioning failed' : agent.status === 'offline' ? 'Agent offline' : 'Starting up...') : 'Send message...'}
        onSend={(text) => onSend(agent.id, text)}
      />
    </div>
  )
}

function BoardScreen({ agents, onOpen, onSend, onOpenMenu }) {
  const groups = ['attention', 'working', 'offline'].map((key) => ({
    key, ...AGENT_SECTION[key], items: agents.filter((a) => agentSection(a) === key),
  }))
  const summary = groups.filter((g) => g.items.length).map((g) => g.items.length + ' ' + g.label.toLowerCase()).join(' · ')

  return (
    <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '10px 16px', borderBottom: '1px solid var(--border)' }}>
        <Icon name="LayoutGrid" size={16} style={{ color: 'var(--muted-foreground)' }} />
        <span style={{ fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)' }}>Agents</span>
        <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-10-5)', color: 'var(--muted-foreground)' }}>{summary}</span>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 4 }}>
          <Button variant="ghost" size="icon-sm" title="More"><Icon name="MoreVertical" size={16} /></Button>
        </div>
      </div>
      {/* Cards cluster under their section rather than forming one flat row, so the
          board answers "what needs me" at the same glance as the sidebar. */}
      <div style={{ flex: 1, minHeight: 0, display: 'flex', gap: 24, padding: 12, overflowX: 'auto' }}>
        {groups.filter((g) => g.items.length > 0).map((g) => (
          <div key={g.key} style={{ display: 'flex', minHeight: 0, flexDirection: 'column', gap: 10, flexShrink: 0 }}>
            {/* The cluster names itself in a tinted band the width of its cards, so
                the grouping is a visible region and not just a gap. */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, borderRadius: 'var(--radius-md)', border: '1px solid color-mix(in srgb, ' + g.color + ' 30%, var(--border))', background: 'color-mix(in srgb, ' + g.color + ' 10%, var(--card))', padding: '6px 10px' }}>
              <Icon name={g.icon} size={13} style={{ color: g.color }} />
              <span style={{ fontSize: 'var(--text-12)', fontWeight: 'var(--weight-semibold)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)', color: 'var(--foreground)' }}>{g.label}</span>
              <span style={{ marginLeft: 'auto', minWidth: 18, textAlign: 'center', borderRadius: 'var(--radius-full)', background: 'color-mix(in srgb, ' + g.color + ' 22%, transparent)', color: g.color, padding: '0 6px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', fontWeight: 'var(--weight-medium)' }}>{g.items.length}</span>
            </div>
            <div style={{ display: 'flex', minHeight: 0, flex: 1, gap: 12 }}>
              {g.items.map((a) => <BoardCard key={a.id} agent={a} onOpen={onOpen} onSend={onSend} />)}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

Object.assign(window, { BoardScreen, BoardCard })
