// Fake but shape-accurate fixtures for the ElasticClaw dashboard recreation.
window.EC_DATA = {
  agents: [
    {
      id: 'a1', name: 'ADV-812/flaky-e2e', template: 'elasticclaw · linear-story',
      status: 'connected', isStreaming: true, accent: 'var(--claw-cyan)', pinned: true,
      unreadCount: 0, contextUsage: 64, uptime: '1h 12m', age: '1h 12m', tags: ['bug-lane', 'repo=elasticclaw'],
      activityLine: 'Bash · go test ./pkg/hub/...',
      openPrs: [
        { repo: 'elasticclaw/elasticclaw', prNumber: 2852, title: 'fix(hub): account for the third retry attempt', url: 'https://github.com/elasticclaw/elasticclaw/pull/2852' },
      ],
      now: { state: 'working', what: 'Bash', detail: 'go test ./pkg/hub/... -run TestClawRetry', elapsed: '12.4s', lastOutput: '3s ago' },
      steps: [
        { title: 'Read', detail: 'pkg/hub/claw_retry_test.go', category: 'read', durationMs: 120 },
        { title: 'Grep', detail: 'demoteStaleRunning', category: 'search', durationMs: 340, result: 'pkg/hub/claw_retry.go:88\npkg/hub/agent_idle.go:210' },
        { title: 'Bash', detail: 'go test ./pkg/hub/... -run TestClawRetry', category: 'run', status: 'failed', exitCode: 1, durationMs: 41200, error: '--- FAIL: TestClawRetryBackoff (0.00s)\n    claw_retry_test.go:142: expected 3 attempts, got 2' },
        { title: 'Edit', detail: 'pkg/hub/claw_retry.go', category: 'edit', status: 'running', durationMs: 2400 },
      ],
      messages: [
        { id: 'm1', role: 'hub', content: 'workflow linear-story matched ADV-812 · workspace elasticclaw' },
        { id: 'm2', role: 'user', userId: 'u1', time: '14:02', content: 'Rebase onto main and re-run the e2e shard.' },
        { id: 'm3', role: 'claw', time: '14:03', content: 'Rebased cleanly. Shard 2 is failing on TestClawRetryBackoff — two parallel tests share one fixture. Patching the retry accounting now.' },
        { id: 'm3b', role: 'user', userId: 'u2', time: '14:06', content: 'Keep the fixture per-test rather than sharing it — we hit this same flake in the release shard last month.' },
        { id: 'm3c', role: 'claw', time: '14:07', content: 'Understood. Giving each subtest its own fixture and re-running the shard 20 times to confirm.' },
      ],
    },
    {
      id: 'a2', name: 'INF-44/bump-deps', template: 'elasticclaw · dependency-updates',
      status: 'provisioning', isStreaming: false, accent: 'var(--claw-indigo)', pinned: false,
      unreadCount: 0, contextUsage: 0, uptime: '—', age: '38s', tags: ['deps'], bootstrapStep: 'workspace',
      activityLine: 'Preparing workspace',
      now: null, steps: [], messages: [],
    },
    {
      id: 'a3', name: 'DOC-9/rewrite-install', template: 'docs · github-issue',
      status: 'idle', isStreaming: false, accent: 'var(--claw-amber)', pinned: false,
      unreadCount: 3, contextUsage: 41, uptime: '4h 06m', age: '4m ago', tags: ['docs'],
      activityLine: 'Waiting on you',
      openPrs: [
        { repo: 'elasticclaw/docs', prNumber: 414, title: 'docs: rewrite the install page for 1.7', url: 'https://github.com/elasticclaw/docs/pull/414' },
        { repo: 'elasticclaw/docs', prNumber: 415, title: 'docs: drop the Docker install section', url: 'https://github.com/elasticclaw/docs/pull/415' },
      ],
      now: { state: 'waiting', what: 'Should I keep the Docker install section or link to the docs site?', lastOutput: '4m ago' },
      steps: [
        { title: 'Read', detail: 'docs/installation.mdx', category: 'read', durationMs: 90 },
        { title: 'Edit', detail: 'docs/installation.mdx', category: 'edit', durationMs: 1400 },
      ],
      messages: [
        { id: 'm4', role: 'user', userId: 'u1', time: '09:41', content: 'Rewrite the install page for the brew tap flow.' },
        { id: 'm5', role: 'claw', time: '09:58', content: 'Draft is in. Should I keep the Docker install section or link to the docs site?' },
        { id: 'm5b', role: 'user', userId: 'u4', time: '10:04', content: 'Link to the docs site — support keeps sending customers there anyway.' },
      ],
    },
    {
      id: 'a4', name: 'SUP-201/escalation', template: 'support · webhook',
      status: 'error', isStreaming: false, accent: 'var(--claw-rose)', pinned: false,
      unreadCount: 0, contextUsage: 12, uptime: '—', age: '22m ago', tags: ['escalation'],
      activityLine: 'daytona quota exceeded',
      now: { state: 'error', what: 'sandbox provisioning failed: daytona quota exceeded', lastOutput: '22m ago' },
      steps: [], messages: [{ id: 'm6', role: 'hub', content: 'attempt 2 of 3 failed · provider daytona' }],
    },
    {
      id: 'a5', name: 'REL-7/changelog', template: 'elasticclaw · release-followup',
      status: 'offline', isStreaming: false, accent: 'var(--claw-slate)', pinned: false,
      unreadCount: 0, contextUsage: 0, uptime: '—', age: '2d ago', tags: ['release'],
      activityLine: 'Sandbox stopped',
      now: null, steps: [], messages: [],
    },
  ],
};

/* The signed-in user and the teammates who share these agent threads. One stable
   colour per person — the thread tells authorship by colour, so it cannot be
   assigned per message. */
window.EC_PEOPLE = {
  me: { id: 'u1', name: 'Marc Oliveira', short: 'You' },
  marc: { id: 'u1', name: 'Marc Oliveira', color: 'var(--chart-1)' },
  priya: { id: 'u2', name: 'Priya Raman', color: 'var(--chart-3)' },
  dana: { id: 'u3', name: 'Dana Whitfield', color: 'var(--chart-2)' },
  sofia: { id: 'u4', name: 'Sofia Braga', color: 'var(--chart-5)' },
};

/* Bridge to the design system's agent-state exports. The kit prefers the real
   components from _ds_bundle.js; the inline definitions below are only the
   fallback for a bundle compiled before AgentStateChip existed, so the grouping
   rule still has exactly one authority (components/agents/AgentStateChip.jsx). */
;(() => {
  const NS = window.ElasticClawDesignSystem_0b1de0 || {}
  window.AGENT_SECTION = NS.AGENT_SECTION || {
    attention: { label: 'Needs attention', icon: 'AlertCircle', color: 'var(--status-idle)', empty: 'Nothing waiting on you' },
    working: { label: 'Working', icon: 'Loader2', color: 'var(--status-connected)', empty: 'No agents running' },
    offline: { label: 'Offline', icon: 'Circle', color: 'var(--status-offline)', empty: 'No stopped agents' },
  }
  window.agentSection = NS.agentSection || ((a) => {
    if (!a) return 'offline'
    if (a.status === 'error') return 'attention'
    if (a.status === 'offline') return 'offline'
    if (a.status === 'idle') return (a.unreadCount > 0 || (a.now && a.now.state === 'waiting')) ? 'attention' : 'offline'
    return 'working'
  })
  const STATE = NS.AGENT_STATE || {
    streaming: { label: 'RUNNING', color: 'var(--status-streaming)' },
    connected: { label: 'READY', color: 'var(--status-connected)' },
    provisioning: { label: 'STARTING', color: 'var(--status-provisioning)' },
    idle: { label: 'IDLE', color: 'var(--status-idle)' },
    offline: { label: 'OFFLINE', color: 'var(--status-offline)' },
    error: { label: 'ERROR', color: 'var(--status-error)' },
  }
  window.AgentStateChip = NS.AgentStateChip || (({ status = 'connected', isStreaming = false, size = 'sm', reason }) => {
    const s = STATE[isStreaming ? 'streaming' : status] || STATE.connected
    const muted = status === 'offline' && !isStreaming
    const lg = size === 'md'
    return React.createElement('span', {
      title: reason,
      style: {
        display: 'inline-flex', alignItems: 'center', gap: 4, flexShrink: 0, width: 'fit-content',
        padding: lg ? '1px 6px' : '0 4px', borderRadius: 'var(--radius-sm)',
        background: muted ? 'var(--muted)' : 'color-mix(in srgb, ' + s.color + ' 16%, transparent)',
        color: s.color, fontFamily: 'var(--font-mono)', fontSize: lg ? 'var(--text-2xs-plus)' : 'var(--text-2xs)',
        fontWeight: 'var(--weight-medium)', letterSpacing: '0.04em', lineHeight: 'var(--leading-4)',
      },
    }, s.label, reason ? React.createElement('span', { style: { opacity: 0.75, letterSpacing: 0 } }, ' · ' + reason) : null)
  })
})();

/* Message authorship, resolved once: is this the signed-in user, a teammate, or
   the agent? Screens render the result; they never decide it themselves. */
window.messageAuthor = (message, agent) => {
  const people = window.EC_PEOPLE
  if (message.role !== 'user') return { self: false, author: agent ? agent.name : 'agent' }
  const person = Object.values(people).find((p) => p.id === message.userId)
  const isSelf = message.userId === people.me.id || !message.userId
  return {
    self: isSelf,
    author: isSelf ? 'You' : (person ? person.name : 'Teammate'),
    authorColor: person && person.color ? person.color : 'var(--chart-3)',
  }
};

/* MessageBubble gained the self / authorColor authorship props; a bundle compiled
   before that would paint a teammate as if it were you. Until it recompiles, put
   teammates on the neutral agent surface instead of the blue "you" one. No copy of
   the component — just a prop rewrite. */
;(() => {
  const NS = window.ElasticClawDesignSystem_0b1de0 || {}
  const Base = NS.MessageBubble
  const supportsAuthorship = String(Base || '').includes('authorColor')
  window.Bubble = supportsAuthorship ? Base : (props) => {
    const teammate = props.role === 'user' && !props.self
    return React.createElement(Base, { ...props, role: teammate ? 'claw' : props.role, accent: teammate ? undefined : props.accent })
  }
})();
