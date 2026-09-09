const DS = window.ElasticClawDesignSystem_0b1de0

function App() {
  const [signedIn, setSignedIn] = React.useState(true)
  const [agents, setAgents] = React.useState(window.EC_DATA.agents)
  const [selectedId, setSelectedId] = React.useState(null)
  const [collapsed, setCollapsed] = React.useState(false)
  const [query, setQuery] = React.useState('')
  const [filters, setFilters] = React.useState(['bug-lane'])
  const [view, setView] = React.useState('agents')

  const visible = agents.filter((a) => !query.trim() || a.name.toLowerCase().includes(query.toLowerCase()) || a.tags.some((t) => t.toLowerCase().includes(query.toLowerCase())))
  const selected = agents.find((a) => a.id === selectedId) || null

  const send = (id, text) => setAgents((prev) => prev.map((a) => a.id === id
    ? { ...a, unreadCount: 0, messages: [...a.messages, { id: 'u' + Date.now(), role: 'user', time: new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }), content: text }] }
    : a))

  const kill = (id) => { setAgents((prev) => prev.filter((a) => a.id !== id)); setSelectedId(null) }
  const togglePin = (id) => setAgents((prev) => prev.map((a) => a.id === id ? { ...a, pinned: !a.pinned } : a))

  if (!signedIn) return <LoginScreen onSignIn={() => setSignedIn(true)} />

  return (
    <div style={{ display: 'flex', height: '100%', background: 'var(--background)' }}>
      <SidebarPanel
        agents={visible} selectedId={selectedId}
        onSelect={(id) => { setSelectedId(id); setAgents((p) => p.map((a) => a.id === id ? { ...a, unreadCount: 0 } : a)) }}
        onTogglePin={togglePin}
        collapsed={collapsed} onToggleCollapse={() => setCollapsed(!collapsed)}
        query={query} onQuery={setQuery}
        filters={filters} onRemoveFilter={(t) => setFilters(filters.filter((x) => x !== t))}
        view={view} onToggleView={() => setView(view === 'agents' ? 'analytics' : 'agents')}
      />
      {view === 'analytics'
        ? <AnalyticsScreen />
        : selected
          ? <ConversationScreen agent={selected} onBack={() => setSelectedId(null)} onSend={send} onKill={kill} />
          : <BoardScreen agents={visible} onOpen={setSelectedId} onSend={send} />}
    </div>
  )
}

Object.assign(window, { App })
