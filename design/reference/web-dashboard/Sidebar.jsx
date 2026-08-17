const DS = window.ElasticClawDesignSystem_0b1de0
const { Button, Input, Icon, AgentRow, NavItem, Tag } = DS
const { AGENT_SECTION, agentSection } = window

function SidebarPanel({ agents, selectedId, onSelect, onTogglePin, collapsed, onToggleCollapse, query, onQuery, filters, onRemoveFilter, view, onToggleView }) {
  if (collapsed) {
    return (
      <aside style={{ width: 'var(--sidebar-width-collapsed)', display: 'flex', flexDirection: 'column', borderRight: '1px solid var(--border)', background: 'var(--card)' }}>
        <div style={{ padding: 8, borderBottom: '1px solid var(--border)', display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4 }}>
          <Button variant="ghost" size="icon-sm" title="Expand sidebar" onClick={onToggleCollapse}><Icon name="PanelLeft" size={16} /></Button>
          <Button variant="ghost" size="icon-sm" title="Create Agent"><Icon name="Plus" size={16} /></Button>
        </div>
        <div style={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4, padding: '8px 0' }}>
          {agents.map((a) => (
            <button key={a.id} onClick={() => onSelect(a.id)} title={a.name}
              style={{ position: 'relative', width: 32, height: 32, display: 'flex', alignItems: 'center', justifyContent: 'center', border: 0, borderRadius: 'var(--radius-md)', cursor: 'pointer', background: a.id === selectedId ? 'var(--accent)' : 'transparent' }}>
              <DS.StatusDot status={a.status} isStreaming={a.isStreaming} />
              {a.unreadCount > 0 && <span style={{ position: 'absolute', top: -2, right: -2, width: 14, height: 14, display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: 8, fontWeight: 700, background: 'var(--unread)', color: '#fff', borderRadius: 'var(--radius-full)' }}>{a.unreadCount}</span>}
            </button>
          ))}
        </div>
      </aside>
    )
  }

  /* Sections come from the design system's own derivation (agentSection), so the
     sidebar and the board can never disagree about what needs attention. Pins
     keep their priority inside a section rather than owning one. */
  const grouped = ['attention', 'working', 'offline'].map((key) => ({
    key, ...AGENT_SECTION[key],
    items: agents.filter((a) => agentSection(a) === key).sort((a, b) => (b.pinned ? 1 : 0) - (a.pinned ? 1 : 0)),
  }))

  return (
    <aside style={{ width: 'var(--sidebar-width)', flexShrink: 0, display: 'flex', flexDirection: 'column', borderRight: '1px solid var(--border)', background: 'var(--card)' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: 16, borderBottom: '1px solid var(--border)' }}>
        <h1 style={{ margin: 0, fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-semibold)', letterSpacing: 'var(--tracking-tight)' }}>ElasticClaw</h1>
        <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
          <Button variant="ghost" size="icon-sm" title="Create Agent"><Icon name="Plus" size={16} /></Button>
          <Button variant="ghost" size="icon-sm" title="Collapse sidebar" onClick={onToggleCollapse}><Icon name="PanelLeftClose" size={16} /></Button>
        </div>
      </div>

      <div style={{ padding: 12, borderBottom: '1px solid var(--border)', display: 'flex', flexDirection: 'column', gap: 8 }}>
        <Input size="sm" icon={<Icon name="Search" size={16} />} placeholder="Filter by name or workflow..." value={query} onChange={(e) => onQuery(e.target.value)} />
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
          <Button variant="outline" size="sm" style={{ height: 28, fontSize: 'var(--text-12)', gap: 4 }}>Tags <Icon name="ChevronDown" size={12} /></Button>
          {filters.map((t) => <Tag key={t} size="md" onRemove={() => onRemoveFilter(t)}>{t}</Tag>)}
        </div>
      </div>

      <div style={{ flex: 1, minHeight: 0, overflowY: 'auto' }}>
        {agents.length === 0
          ? <p style={{ textAlign: 'center', padding: '32px 0', fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>No agents found</p>
          : grouped.map((sec) => (
            <div key={sec.key} style={{ paddingBottom: 4 }}>
              {/* Sticky header so the section a row belongs to stays legible while scrolling. */}
              <div style={{ position: 'sticky', top: 0, zIndex: 1, display: 'flex', alignItems: 'center', gap: 8, background: 'color-mix(in srgb, ' + sec.color + ' 10%, var(--card))', borderTop: '1px solid var(--border)', borderBottom: '1px solid color-mix(in srgb, ' + sec.color + ' 30%, var(--border))', padding: '7px 16px 7px 12px' }}>
                <span style={{ width: 3, alignSelf: 'stretch', flexShrink: 0, borderRadius: 'var(--radius-full)', background: sec.color }} />
                <Icon name={sec.icon} size={13} style={{ color: sec.color }} />
                <span style={{ fontSize: 'var(--text-12)', fontWeight: 'var(--weight-semibold)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-wider)', color: 'var(--foreground)' }}>{sec.label}</span>
                <span style={{ marginLeft: 'auto', minWidth: 18, textAlign: 'center', borderRadius: 'var(--radius-full)', background: 'color-mix(in srgb, ' + sec.color + ' 22%, transparent)', color: sec.color, padding: '0 6px', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-2xs-plus)', fontWeight: 'var(--weight-medium)' }}>{sec.items.length}</span>
              </div>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4, padding: '8px 8px 4px' }}>
                {sec.items.length === 0
                  ? <p style={{ margin: 0, padding: '6px 8px 4px', fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{sec.empty}</p>
                  : sec.items.map((a) => <AgentRow key={a.id} {...a} selected={a.id === selectedId} onClick={() => onSelect(a.id)} onTogglePin={() => onTogglePin(a.id)} />)}
              </div>
            </div>
          ))}
      </div>

      <div style={{ padding: 8, borderTop: '1px solid var(--border)' }}>
        <NavItem icon={<Icon name={view === 'analytics' ? 'LayoutGrid' : 'BarChart3'} size={16} />} label={view === 'analytics' ? 'Agents' : 'Analytics'} active={view === 'analytics'} onClick={onToggleView} />
        <NavItem icon={<Icon name="Settings" size={16} />} label="Settings" />
        <div style={{ margin: '4px 0', borderTop: '1px solid var(--border)' }} />
        <NavItem icon={<Icon name="LogOut" size={16} />} label="Sign out" tone="danger" />
      </div>
    </aside>
  )
}

Object.assign(window, { SidebarPanel })
