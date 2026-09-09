// Fixtures shaped after AnalyticsEffectiveness / CostOverview / TaskRunSummary
// in web/lib/types.ts. Deterministic pseudo-random so the screen is stable.
(() => {
  let seed = 7
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff

  const iso = (d) => d.toISOString().slice(0, 10)
  const today = new Date('2026-08-15T00:00:00Z')

  // 30-day period series
  const days = Array.from({ length: 30 }, (_, i) => {
    const d = new Date(today); d.setUTCDate(d.getUTCDate() - (29 - i)); return iso(d)
  })

  const outcomesByDay = days.map((date) => {
    const clean = 6 + Math.round(rnd() * 12)
    return { date, clean, humanInTheLoop: 1 + Math.round(rnd() * 5), warning: Math.round(rnd() * 4), failed: Math.round(rnd() * 3) }
  })

  const ticketsByDay = days.map((date) => ({
    date, delivered: 2 + Math.round(rnd() * 7), inProgress: Math.round(rnd() * 3), failed: Math.round(rnd() * 2),
  }))

  const models = ['claude-sonnet-4-6', 'claude-opus-4-1', 'claude-haiku-4-5']
  const dailyByModel = days.map((date) => ({
    date,
    'claude-sonnet-4-6': 12 + rnd() * 22,
    'claude-opus-4-1': 4 + rnd() * 14,
    'claude-haiku-4-5': 1 + rnd() * 4,
    Other: rnd() * 3,
  }))

  const workflows = ['linear-story', 'dependency-updates', 'github-issue', 'release-followup']
  const workflowSeries = days.map((date) => ({
    date,
    'linear-story': 8 + rnd() * 14,
    'dependency-updates': 4 + rnd() * 8,
    'github-issue': 2 + rnd() * 7,
    'release-followup': 1 + rnd() * 4,
  }))

  // Trailing-year heatmap: 52 weeks × 7 days, Monday-first, like useHeatmap().
  const end = new Date(today)
  const start = new Date(end); start.setUTCDate(start.getUTCDate() - 363 - ((start.getUTCDay() + 6) % 7))
  const heatDays = Array.from({ length: 364 }, (_, i) => {
    const d = new Date(start); d.setUTCDate(d.getUTCDate() + i)
    const dow = (d.getUTCDay() + 6) % 7
    const weekend = dow > 4
    const cost = weekend ? rnd() * 14 : rnd() * 96
    return { iso: iso(d), cost: rnd() > 0.08 ? cost : 0, runs: Math.round(cost / 4), week: Math.floor(i / 7), day: i % 7 }
  })
  const monthLabels = heatDays
    .filter((d, i) => i === 0 || d.iso.slice(5, 7) !== heatDays[i - 1].iso.slice(5, 7))
    .map((d) => ({ week: d.week, label: new Date(d.iso + 'T00:00:00Z').toLocaleString('en', { month: 'short', timeZone: 'UTC' }) }))

  window.EC_ANALYTICS = {
    days, models, workflows,
    outcomesByDay, ticketsByDay, dailyByModel, workflowSeries,
    heatDays, monthLabels,
    maxHeatCost: Math.max(...heatDays.map((d) => d.cost)),
    funnel: { agentStarted: 318, prOpened: 241, prFinished: 198 },
    runsPerTicket: [{ bucket: '1', tickets: 61 }, { bucket: '2', tickets: 22 }, { bucket: '3+', tickets: 13 }],
    costPerMergedPr: {
      weekly: [
        { weekStart: '2026-07-14', costPerMergedPr: 9.42 },
        { weekStart: '2026-07-21', costPerMergedPr: 8.13 },
        { weekStart: '2026-07-28', costPerMergedPr: 10.24 },
        { weekStart: '2026-08-04', costPerMergedPr: 6.81 },
        { weekStart: '2026-08-11', costPerMergedPr: 6.07 },
      ],
      average: 8.13,
    },
    topTicketsByCost: [
      { issueId: 'ADV-812', issueTitle: 'Flaky e2e shard on hub retry', costUsd: 38.2, runs: 4, outcome: 'merged' },
      { issueId: 'SUP-201', issueTitle: 'Customer escalation: sandbox quota', costUsd: 24.9, runs: 3, outcome: 'failed' },
      { issueId: 'INF-44', issueTitle: 'Dependency bump wave', costUsd: 19.4, runs: 2, outcome: 'merged' },
      { issueId: 'DOC-9', issueTitle: 'Rewrite the install page', costUsd: 11.7, runs: 2, outcome: 'warning' },
      { issueId: 'REL-7', issueTitle: 'Release follow-up: changelog', costUsd: 7.3, runs: 1, outcome: 'merged' },
    ],
    kpis: {
      runs: { value: '318', change: 0.081 },
      uniqueTickets: { value: '96', change: 0.124 },
      successRuns: { value: '78.3%', change: 0.042 },
      successTickets: { value: '91.2%', change: 0.018 },
      ticketToPr: { value: '46m', change: -0.083 },
      readyToMerge: { value: '3.2h', change: -0.151 },
      totalCost: { value: '$1,284', change: 0.094 },
      costPerRun: { value: '$4.04', change: -0.031 },
      costPerTicket: { value: '$13.38', change: 0.061 },
    },
    runsTable: [
      {
        runId: 'run_8f21c4', traceId: '8f21c4b0e7d34a19c5c2f7a1d0b93e44', ownerDisplayName: 'Adversary Labs bug lane', status: 'clean', phase: 'finished',
        issueId: 'ADV-812', issueTitle: 'Flaky e2e shard on hub retry',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'bug-lane',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 12.40, inputTokens: 486_200, outputTokens: 71_400, totalTokens: 557_600,
        duration: '38m', started: '8/15/2026', startedAt: 'Aug 15, 09:12',
        warningTypes: [], failureType: null, humanInteractionCount: 0,
        prCount: 1, mergedPrCount: 1,
        timing: [
          { label: 'Queued wait', value: '1m 12s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '31m', title: 'Time from the agent starting to the PR being opened.' },
          { label: 'Ready to merge', value: '2h 40m', title: 'Business-hours time in your timezone from ready-for-review to merge.' },
        ],
        prs: [{ id: 'pr1', state: 'merged', repo: 'elasticclaw/elasticclaw', prNumber: 2841, url: 'https://github.com/elasticclaw/elasticclaw/pull/2841' }],
        attempts: [{ id: 'a1', attemptNumber: 1, status: 'finished', duration: '38m', failureType: null }],
        events: [
          { id: 'e1', eventType: 'run_queued', time: 'Aug 15, 09:11', actor: 'linear', source: 'linear' },
          { id: 'e2', eventType: 'agent_started', time: 'Aug 15, 09:12', actor: 'elasticclaw', source: 'hub' },
          { id: 'e3', eventType: 'pr_opened', time: 'Aug 15, 09:43', actor: 'elasticclaw-bot', source: 'github' },
          { id: 'e4', eventType: 'ci_succeeded', time: 'Aug 15, 09:58', actor: 'github-actions', source: 'github' },
          { id: 'e5', eventType: 'pr_merged', time: 'Aug 15, 12:23', actor: 'marc', source: 'github' },
        ],
        activity: [
          { title: 'Read', detail: 'pkg/hub/claw_retry_test.go', category: 'read', durationMs: 120 },
          { title: 'Grep', detail: 'demoteStaleRunning', category: 'search', durationMs: 340 },
          { title: 'Bash', detail: 'go test ./pkg/hub/... -run TestClawRetry', category: 'run', status: 'failed', exitCode: 1, durationMs: 41200, error: '--- FAIL: TestClawRetryBackoff (0.00s)\n    claw_retry_test.go:142: expected 3 attempts, got 2' },
          { title: 'Edit', detail: 'pkg/hub/claw_retry.go', category: 'edit', durationMs: 2400 },
          { title: 'Bash', detail: 'go test ./pkg/hub/...', category: 'run', durationMs: 38900, result: 'ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t38.902s' },
        ],
        outputs: [
          {
            stage: 'verify', outputName: 'go test ./...', attemptId: 'attempt_1', exitCode: 0,
            spanId: '4f2c9a1b7e3d5081', spanKind: 'SERVER', durationMs: 38902, status: 'OK',
            stdout: 'ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t38.902s\nok  \tgithub.com/elasticclaw/elasticclaw/pkg/types\t1.204s',
            stderr: '',
            records: [
              { t: '09:31:04.118', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'verify', 'process.command': 'go test ./...' } },
              { t: '09:31:42.902', sev: 'INFO', body: 'ok github.com/elasticclaw/elasticclaw/pkg/hub', attrs: { 'test.package': 'pkg/hub', 'test.duration_ms': 38902 } },
              { t: '09:31:44.106', sev: 'INFO', body: 'ok github.com/elasticclaw/elasticclaw/pkg/types', attrs: { 'test.package': 'pkg/types', 'test.duration_ms': 1204 } },
              { t: '09:31:44.110', sev: 'INFO', body: 'stage finished', attrs: { 'process.exit_code': 0 } },
            ],
          },
          {
            stage: 'verify', outputName: 'golangci-lint run', attemptId: 'attempt_1', exitCode: 0,
            spanId: '9b71e04c2af6338d', spanKind: 'INTERNAL', durationMs: 6410, status: 'OK',
            stdout: '0 issues.', stderr: '',
            records: [
              { t: '09:31:45.002', sev: 'DEBUG', body: 'loading 214 packages', attrs: { 'linter.config': '.golangci.yml' } },
              { t: '09:31:51.412', sev: 'INFO', body: '0 issues.', attrs: { 'linter.issues': 0 } },
            ],
          },
        ],
      },
      {
        runId: 'run_2a90de', traceId: '2a90de77c1b84f0e93a6d2ee5f1c8b30', ownerDisplayName: 'Docs queue', status: 'human_in_the_loop', phase: 'finished',
        issueId: 'DOC-9', issueTitle: 'Rewrite the install page',
        workflowName: 'github-issue', workspaceName: 'docs', factoryName: 'docs-queue',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/docs',
        cost: 5.82, inputTokens: 214_800, outputTokens: 38_100, totalTokens: 252_900,
        duration: '1h 12m', started: '8/15/2026', startedAt: 'Aug 15, 08:04',
        warningTypes: [], failureType: null, humanInteractionCount: 3,
        prCount: 1, mergedPrCount: 1,
        timing: [
          { label: 'Queued wait', value: '48s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '52m', title: 'Time from the agent starting to the PR being opened.' },
          { label: 'Ready to merge', value: '5h 06m', title: 'Business-hours time in your timezone from ready-for-review to merge.' },
        ],
        prs: [{ id: 'pr2', state: 'merged', repo: 'elasticclaw/docs', prNumber: 412, url: 'https://github.com/elasticclaw/docs/pull/412' }],
        attempts: [{ id: 'a2', attemptNumber: 1, status: 'finished', duration: '1h 12m', failureType: null }],
        events: [
          { id: 'e6', eventType: 'agent_started', time: 'Aug 15, 08:04', actor: 'elasticclaw', source: 'hub' },
          { id: 'e7', eventType: 'pr_opened', time: 'Aug 15, 08:56', actor: 'elasticclaw-bot', source: 'github' },
          { id: 'e8', eventType: 'review_commented', time: 'Aug 15, 10:31', actor: 'marc', source: 'github' },
          { id: 'e9', eventType: 'pr_merged', time: 'Aug 15, 14:02', actor: 'marc', source: 'github' },
        ],
        activity: [
          { title: 'Read', detail: 'docs/installation.mdx', category: 'read', durationMs: 90 },
          { title: 'Edit', detail: 'docs/installation.mdx', category: 'edit', durationMs: 1400 },
          { title: 'WebFetch', detail: 'https://elasticclaw.ai/docs/installation', category: 'web', durationMs: 2100 },
        ],
        outputs: [{
          stage: 'build', outputName: 'npm run build', attemptId: 'attempt_1', exitCode: 0,
          spanId: 'c0d4471aa9e2b615', spanKind: 'SERVER', durationMs: 24100, status: 'OK',
          stdout: 'Compiled successfully in 24.1s', stderr: '',
          records: [
            { t: '08:41:02.400', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'build', 'process.command': 'npm run build' } },
            { t: '08:41:19.880', sev: 'WARN', body: 'unused export in docs/installation.mdx', attrs: { 'code.filepath': 'docs/installation.mdx', 'code.lineno': 84 } },
            { t: '08:41:26.500', sev: 'INFO', body: 'Compiled successfully in 24.1s', attrs: { 'build.artifacts': 41 } },
          ],
        }],
      },
      {
        runId: 'run_5c33bb', traceId: '5c33bb41f9a24d67b8e0c3a7d5142f9e', ownerDisplayName: 'Support escalations', status: 'failed', phase: 'provisioning',
        issueId: 'SUP-201', issueTitle: 'Customer escalation: sandbox quota',
        workflowName: 'webhook-escalation', workspaceName: 'support', factoryName: 'release',
        model: 'claude-opus-4-1', repo: 'elasticclaw/elasticclaw',
        cost: 9.14, inputTokens: 302_500, outputTokens: 44_900, totalTokens: 347_400,
        duration: '22m', started: '8/14/2026', startedAt: 'Aug 14, 16:38',
        warningTypes: ['provider_degraded'], failureType: 'sandbox_provision_failed', humanInteractionCount: 1,
        prCount: 0, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '22m', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
        ],
        prs: [],
        attempts: [
          { id: 'a3', attemptNumber: 1, status: 'failed', duration: '8m', failureType: 'sandbox_provision_failed' },
          { id: 'a4', attemptNumber: 2, status: 'failed', duration: '14m', failureType: 'sandbox_provision_failed' },
        ],
        events: [
          { id: 'e10', eventType: 'run_queued', time: 'Aug 14, 16:38', actor: 'webhook', source: 'webhook' },
          { id: 'e11', eventType: 'provision_failed', time: 'Aug 14, 16:46', actor: 'daytona', source: 'provider' },
          { id: 'e12', eventType: 'attempt_retried', time: 'Aug 14, 16:46', actor: 'elasticclaw', source: 'hub' },
          { id: 'e13', eventType: 'provision_failed', time: 'Aug 14, 17:00', actor: 'daytona', source: 'provider' },
        ],
        activity: [],
        outputs: [{
          stage: 'provision', outputName: 'daytona create', attemptId: 'attempt_2', exitCode: 1,
          spanId: '2e8fbb50d1c47a93', spanKind: 'CLIENT', durationMs: 14204, status: 'ERROR',
          stdout: '', stderr: 'error: quota exceeded for workspace pool "default" (limit 12)',
          records: [
            { t: '16:46:12.004', sev: 'INFO', body: 'requesting sandbox', attrs: { 'cloud.provider': 'daytona', 'sandbox.pool': 'default' } },
            { t: '16:46:18.771', sev: 'WARN', body: 'provider reported degraded capacity', attrs: { 'cloud.region': 'us-east-1', 'retry.attempt': 1 } },
            { t: '16:46:26.208', sev: 'ERROR', body: 'quota exceeded for workspace pool "default" (limit 12)', attrs: { 'error.type': 'quota_exceeded', 'sandbox.pool.limit': 12, 'process.exit_code': 1 } },
            { t: '16:46:26.210', sev: 'FATAL', body: 'stage aborted, no sandbox available', attrs: { 'stage.id': 'provision' } },
          ],
        }],
      },
      {
        runId: 'run_7d14aa', traceId: '7d14aa2c60f741b9ae35d8c1b7902e6f', ownerDisplayName: 'Dependency updates', status: 'warning', phase: 'finished',
        issueId: 'INF-44', issueTitle: 'Dependency bump wave',
        workflowName: 'dependency-updates', workspaceName: 'elasticclaw', factoryName: 'release',
        model: 'claude-haiku-4-5', repo: 'elasticclaw/elasticclaw',
        cost: 1.96, inputTokens: 88_400, outputTokens: 12_600, totalTokens: 101_000,
        duration: '14m', started: '8/14/2026', startedAt: 'Aug 14, 06:02',
        warningTypes: ['dashboard_intervention'], failureType: null, humanInteractionCount: 2,
        prCount: 2, mergedPrCount: 1,
        timing: [
          { label: 'Queued wait', value: '36s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '11m', title: 'Time from the agent starting to the PR being opened.' },
          { label: 'Ready to merge', value: '1h 18m', title: 'Business-hours time in your timezone from ready-for-review to merge.' },
        ],
        prs: [
          { id: 'pr3', state: 'merged', repo: 'elasticclaw/elasticclaw', prNumber: 2830, url: 'https://github.com/elasticclaw/elasticclaw/pull/2830' },
          { id: 'pr4', state: 'open', repo: 'elasticclaw/elasticclaw', prNumber: 2831, url: 'https://github.com/elasticclaw/elasticclaw/pull/2831' },
        ],
        attempts: [{ id: 'a5', attemptNumber: 1, status: 'finished', duration: '14m', failureType: null }],
        events: [
          { id: 'e14', eventType: 'agent_started', time: 'Aug 14, 06:02', actor: 'elasticclaw', source: 'hub' },
          { id: 'e15', eventType: 'pr_opened', time: 'Aug 14, 06:13', actor: 'elasticclaw-bot', source: 'github' },
          { id: 'e16', eventType: 'dashboard_message_sent', time: 'Aug 14, 06:20', actor: 'marc', source: 'hub' },
          { id: 'e17', eventType: 'pr_merged', time: 'Aug 14, 07:31', actor: 'marc', source: 'github' },
        ],
        activity: [
          { title: 'Bash', detail: 'go get -u ./...', category: 'run', durationMs: 18400 },
          { title: 'Edit', detail: 'go.mod', category: 'edit', durationMs: 800 },
        ],
        outputs: [{
          stage: 'verify', outputName: 'go build ./...', attemptId: 'attempt_1', exitCode: 0,
          spanId: '7a3311fe08c9d240', spanKind: 'SERVER', durationMs: 9120, status: 'OK',
          stdout: '', stderr: '',
          records: [
            { t: '06:12:41.900', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'verify', 'process.command': 'go build ./...' } },
            { t: '06:12:51.020', sev: 'INFO', body: 'stage finished', attrs: { 'process.exit_code': 0 } },
          ],
        }],
      },
      {
        runId: 'run_9e02f7', traceId: '9e02f7d3a5c8412fb61e04d97c3a58bb', ownerDisplayName: 'Release follow-up', status: 'clean', phase: 'finished',
        issueId: 'REL-7', issueTitle: 'Release follow-up: changelog',
        workflowName: 'release-followup', workspaceName: 'elasticclaw', factoryName: 'release',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 7.31, inputTokens: 190_300, outputTokens: 26_700, totalTokens: 217_000,
        duration: '51m', started: '8/13/2026', startedAt: 'Aug 13, 18:20',
        warningTypes: [], failureType: null, humanInteractionCount: 0,
        prCount: 1, mergedPrCount: 1,
        timing: [
          { label: 'Queued wait', value: '54s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '44m', title: 'Time from the agent starting to the PR being opened.' },
          { label: 'Ready to merge', value: '0s', title: 'Business-hours time in your timezone from ready-for-review to merge.' },
        ],
        prs: [{ id: 'pr5', state: 'merged', repo: 'elasticclaw/elasticclaw', prNumber: 2822, url: 'https://github.com/elasticclaw/elasticclaw/pull/2822' }],
        attempts: [{ id: 'a6', attemptNumber: 1, status: 'finished', duration: '51m', failureType: null }],
        events: [
          { id: 'e18', eventType: 'agent_started', time: 'Aug 13, 18:20', actor: 'elasticclaw', source: 'hub' },
          { id: 'e19', eventType: 'pr_merged', time: 'Aug 13, 19:11', actor: 'elasticclaw-bot', source: 'github' },
        ],
        activity: [{ title: 'Edit', detail: 'CHANGELOG.md', category: 'edit', durationMs: 1100 }],
        outputs: [],
      },
      {
        runId: 'run_1b77c0', traceId: '1b77c05eab924d3082f7c61de40395aa', ownerDisplayName: 'Adversary Labs bug lane', status: 'running', phase: 'implementing',
        issueId: 'ADV-806', issueTitle: 'Sidebar drag reorder drops pinned agents',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'bug-lane',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 10.08, inputTokens: 401_700, outputTokens: 52_300, totalTokens: 454_000,
        duration: '44m', started: '8/13/2026', startedAt: 'Aug 13, 11:45',
        warningTypes: [], failureType: null, humanInteractionCount: 0,
        prCount: 0, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '1m 04s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
        ],
        prs: [],
        attempts: [{ id: 'a7', attemptNumber: 1, status: 'running', duration: '44m', failureType: null }],
        events: [
          { id: 'e20', eventType: 'run_queued', time: 'Aug 13, 11:44', actor: 'linear', source: 'linear' },
          { id: 'e21', eventType: 'agent_started', time: 'Aug 13, 11:45', actor: 'elasticclaw', source: 'hub' },
        ],
        activity: [
          { title: 'Read', detail: 'web/components/sidebar.tsx', category: 'read', durationMs: 210 },
          { title: 'Edit', detail: 'web/components/sidebar.tsx', category: 'edit', status: 'running', durationMs: 3200 },
        ],
        outputs: [],
      },
      {
        runId: 'run_c41d90', traceId: 'c41d9077b2e845a3916fd8c4e5027ab1', ownerDisplayName: 'Adversary Labs bug lane', status: 'human_in_the_loop', phase: 'finished',
        issueId: 'ADV-812', issueTitle: 'Flaky e2e shard on hub retry',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'bug-lane',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 8.20, inputTokens: 318_400, outputTokens: 47_900, totalTokens: 366_300,
        duration: '1h 04m', started: '8/14/2026', startedAt: 'Aug 14, 14:10',
        warningTypes: [], failureType: null, humanInteractionCount: 2,
        prCount: 1, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '52s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '58m', title: 'Time from the agent starting to the PR being opened.' },
        ],
        prs: [{ id: 'pr8', state: 'closed', repo: 'elasticclaw/elasticclaw', prNumber: 2839, url: 'https://github.com/elasticclaw/elasticclaw/pull/2839' }],
        attempts: [{ id: 'a10', attemptNumber: 1, status: 'finished', duration: '1h 04m', failureType: null }],
        events: [
          { id: 'e30', eventType: 'agent_started', time: 'Aug 14, 14:10', actor: 'elasticclaw', source: 'hub' },
          { id: 'e31', eventType: 'pr_opened', time: 'Aug 14, 15:08', actor: 'elasticclaw-bot', source: 'github' },
          { id: 'e32', eventType: 'review_commented', time: 'Aug 14, 16:22', actor: 'priya', source: 'github' },
          { id: 'e33', eventType: 'pr_closed', time: 'Aug 14, 16:40', actor: 'priya', source: 'github' },
        ],
        activity: [
          { title: 'Read', detail: 'pkg/hub/claw_retry.go', category: 'read', durationMs: 140 },
          { title: 'Edit', detail: 'pkg/hub/claw_retry.go', category: 'edit', durationMs: 2100 },
          { title: 'Bash', detail: 'go test ./pkg/hub/... -run TestClawRetry -count 20', category: 'run', durationMs: 184000 },
        ],
        outputs: [{
          stage: 'verify', outputName: 'go test -count 20', attemptId: 'attempt_1', exitCode: 0,
          spanId: 'a5c1907be3d4f206', spanKind: 'SERVER', durationMs: 184002, status: 'OK',
          stdout: 'ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t184.002s', stderr: '',
          records: [
            { t: '15:02:11.004', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'verify', 'process.command': 'go test -count 20' } },
            { t: '15:05:15.006', sev: 'INFO', body: 'ok github.com/elasticclaw/elasticclaw/pkg/hub', attrs: { 'test.package': 'pkg/hub', 'test.runs': 20 } },
          ],
        }],
      },
      {
        runId: 'run_6b21f8', traceId: '6b21f8ac04e7429db3157ce0f8a6d215', ownerDisplayName: 'Adversary Labs bug lane', status: 'warning', phase: 'finished',
        issueId: 'ADV-812', issueTitle: 'Flaky e2e shard on hub retry',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'bug-lane',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 8.40, inputTokens: 296_100, outputTokens: 41_200, totalTokens: 337_300,
        duration: '47m', started: '8/13/2026', startedAt: 'Aug 13, 10:02',
        warningTypes: ['dashboard_intervention'], failureType: null, humanInteractionCount: 1,
        prCount: 1, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '1m 40s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '41m', title: 'Time from the agent starting to the PR being opened.' },
        ],
        prs: [{ id: 'pr9', state: 'closed', repo: 'elasticclaw/elasticclaw', prNumber: 2836, url: 'https://github.com/elasticclaw/elasticclaw/pull/2836' }],
        attempts: [{ id: 'a11', attemptNumber: 1, status: 'finished', duration: '47m', failureType: null }],
        events: [
          { id: 'e34', eventType: 'agent_started', time: 'Aug 13, 10:02', actor: 'elasticclaw', source: 'hub' },
          { id: 'e35', eventType: 'pr_opened', time: 'Aug 13, 10:43', actor: 'elasticclaw-bot', source: 'github' },
          { id: 'e36', eventType: 'dashboard_message_sent', time: 'Aug 13, 10:51', actor: 'marc', source: 'hub' },
          { id: 'e37', eventType: 'pr_closed', time: 'Aug 13, 11:12', actor: 'marc', source: 'github' },
        ],
        activity: [
          { title: 'Grep', detail: 'demoteStaleRunning', category: 'search', durationMs: 410 },
          { title: 'Edit', detail: 'pkg/hub/claw_status.go', category: 'edit', durationMs: 1800 },
        ],
        outputs: [{
          stage: 'verify', outputName: 'go test ./pkg/hub/...', attemptId: 'attempt_1', exitCode: 0,
          spanId: 'd80b3f16ca594e77', spanKind: 'SERVER', durationMs: 40120, status: 'OK',
          stdout: 'ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t40.120s', stderr: '',
          records: [
            { t: '10:41:02.100', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'verify', 'process.command': 'go test ./pkg/hub/...' } },
            { t: '10:41:31.900', sev: 'WARN', body: 'test retried after transient failure', attrs: { 'test.name': 'TestClawRetryBackoff', 'retry.attempt': 1 } },
            { t: '10:41:42.220', sev: 'INFO', body: 'stage finished', attrs: { 'process.exit_code': 0 } },
          ],
        }],
      },
      {
        runId: 'run_3c05a1', traceId: '3c05a1b8f7e2461da9038c5b1e70d4f6', ownerDisplayName: 'Adversary Labs bug lane', status: 'failed', phase: 'verifying',
        issueId: 'ADV-812', issueTitle: 'Flaky e2e shard on hub retry',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'bug-lane',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 9.20, inputTokens: 341_700, outputTokens: 39_800, totalTokens: 381_500,
        duration: '1h 18m', started: '8/12/2026', startedAt: 'Aug 12, 15:04',
        warningTypes: [], failureType: 'verification_failed', humanInteractionCount: 0,
        prCount: 0, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '1m 08s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '1h 16m', title: 'Time from the agent starting to the PR being opened.' },
        ],
        prs: [],
        attempts: [
          { id: 'a12', attemptNumber: 1, status: 'failed', duration: '34m', failureType: 'verification_failed' },
          { id: 'a13', attemptNumber: 2, status: 'failed', duration: '44m', failureType: 'verification_failed' },
        ],
        events: [
          { id: 'e38', eventType: 'run_queued', time: 'Aug 12, 15:03', actor: 'linear', source: 'linear' },
          { id: 'e39', eventType: 'agent_started', time: 'Aug 12, 15:04', actor: 'elasticclaw', source: 'hub' },
          { id: 'e40', eventType: 'attempt_retried', time: 'Aug 12, 15:38', actor: 'elasticclaw', source: 'hub' },
          { id: 'e41', eventType: 'verification_failed', time: 'Aug 12, 16:22', actor: 'elasticclaw', source: 'hub' },
        ],
        activity: [
          { title: 'Read', detail: 'pkg/hub/claw_retry_test.go', category: 'read', durationMs: 130 },
          { title: 'Bash', detail: 'go test ./pkg/hub/... -run TestClawRetry -count 50', category: 'run', status: 'failed', exitCode: 1, durationMs: 402100, error: '--- FAIL: TestClawRetryBackoff (0.00s)\n    claw_retry_test.go:142: expected 3 attempts, got 2' },
        ],
        outputs: [{
          stage: 'verify', outputName: 'go test -count 50', attemptId: 'attempt_2', exitCode: 1,
          spanId: 'f13aa9c5027be684', spanKind: 'SERVER', durationMs: 402104, status: 'ERROR',
          stdout: '', stderr: '--- FAIL: TestClawRetryBackoff (0.00s)\n    claw_retry_test.go:142: expected 3 attempts, got 2\nFAIL\tgithub.com/elasticclaw/elasticclaw/pkg/hub\t402.104s',
          records: [
            { t: '15:38:04.000', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'verify', 'process.command': 'go test -count 50' } },
            { t: '15:44:12.880', sev: 'WARN', body: 'flaky test detected, 3 of 50 iterations failed', attrs: { 'test.name': 'TestClawRetryBackoff', 'test.flake_rate': 0.06 } },
            { t: '15:44:46.104', sev: 'ERROR', body: 'expected 3 attempts, got 2', attrs: { 'code.filepath': 'pkg/hub/claw_retry_test.go', 'code.lineno': 142, 'process.exit_code': 1 } },
          ],
        }],
      },
      {
        runId: 'run_4a12bd', traceId: '4a12bd39ce6748a0b7f21d5c908e3aa7', ownerDisplayName: 'Platform API lane', status: 'clean', phase: 'ready_to_merge',
        issueId: 'PLT-31', issueTitle: 'Rate limit headers on the public API',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'release',
        model: 'claude-sonnet-4-6', repo: 'elasticclaw/elasticclaw',
        cost: 6.75, inputTokens: 254_300, outputTokens: 33_600, totalTokens: 287_900,
        duration: '42m', started: '8/15/2026', startedAt: 'Aug 15, 11:20',
        warningTypes: [], failureType: null, humanInteractionCount: 0,
        prCount: 1, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '44s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '39m', title: 'Time from the agent starting to the PR being opened.' },
        ],
        prs: [{ id: 'pr6', state: 'open', repo: 'elasticclaw/elasticclaw', prNumber: 2848, url: 'https://github.com/elasticclaw/elasticclaw/pull/2848' }],
        attempts: [{ id: 'a8', attemptNumber: 1, status: 'finished', duration: '42m', failureType: null }],
        events: [
          { id: 'e22', eventType: 'run_queued', time: 'Aug 15, 11:19', actor: 'linear', source: 'linear' },
          { id: 'e23', eventType: 'agent_started', time: 'Aug 15, 11:20', actor: 'elasticclaw', source: 'hub' },
          { id: 'e24', eventType: 'pr_opened', time: 'Aug 15, 11:59', actor: 'elasticclaw-bot', source: 'github' },
          { id: 'e25', eventType: 'ci_succeeded', time: 'Aug 15, 12:14', actor: 'github-actions', source: 'github' },
        ],
        activity: [
          { title: 'Read', detail: 'pkg/api/middleware.go', category: 'read', durationMs: 160 },
          { title: 'Edit', detail: 'pkg/api/middleware.go', category: 'edit', durationMs: 2600 },
          { title: 'Bash', detail: 'go test ./pkg/api/...', category: 'run', durationMs: 22400, result: 'ok  \tgithub.com/elasticclaw/elasticclaw/pkg/api\t22.401s' },
        ],
        outputs: [{
          stage: 'verify', outputName: 'go test ./pkg/api/...', attemptId: 'attempt_1', exitCode: 0,
          spanId: 'b62f0d8471ac93e5', spanKind: 'SERVER', durationMs: 22401, status: 'OK',
          stdout: 'ok  \tgithub.com/elasticclaw/elasticclaw/pkg/api\t22.401s', stderr: '',
          records: [
            { t: '11:56:02.400', sev: 'INFO', body: 'stage started', attrs: { 'stage.id': 'verify', 'process.command': 'go test ./pkg/api/...' } },
            { t: '11:56:24.801', sev: 'INFO', body: 'ok github.com/elasticclaw/elasticclaw/pkg/api', attrs: { 'test.package': 'pkg/api', 'test.duration_ms': 22401 } },
          ],
        }],
      },
      {
        runId: 'run_d90c33', traceId: 'd90c3358af1b40e7962c4d70e5138bba', ownerDisplayName: 'Platform API lane', status: 'failed', phase: 'implementing',
        issueId: 'PLT-31', issueTitle: 'Rate limit headers on the public API',
        workflowName: 'linear-story', workspaceName: 'elasticclaw', factoryName: 'release',
        model: 'claude-haiku-4-5', repo: 'elasticclaw/elasticclaw',
        cost: 3.10, inputTokens: 122_800, outputTokens: 18_400, totalTokens: 141_200,
        duration: '19m', started: '8/14/2026', startedAt: 'Aug 14, 09:31',
        warningTypes: [], failureType: 'context_exhausted', humanInteractionCount: 1,
        prCount: 0, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '38s', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
          { label: 'Implementation', value: '18m', title: 'Time from the agent starting to the PR being opened.' },
        ],
        prs: [],
        attempts: [{ id: 'a9', attemptNumber: 1, status: 'failed', duration: '19m', failureType: 'context_exhausted' }],
        events: [
          { id: 'e26', eventType: 'agent_started', time: 'Aug 14, 09:31', actor: 'elasticclaw', source: 'hub' },
          { id: 'e27', eventType: 'dashboard_message_sent', time: 'Aug 14, 09:44', actor: 'priya', source: 'hub' },
          { id: 'e28', eventType: 'context_exhausted', time: 'Aug 14, 09:50', actor: 'elasticclaw', source: 'hub' },
        ],
        activity: [{ title: 'Read', detail: 'pkg/api/', category: 'read', durationMs: 980 }],
        outputs: [{
          stage: 'implement', outputName: 'agent turn', attemptId: 'attempt_1', exitCode: 1,
          spanId: '3ea70c15bd8492f0', spanKind: 'INTERNAL', durationMs: 1132000, status: 'ERROR',
          stdout: '', stderr: 'error: context window exhausted after 41 tool calls',
          records: [
            { t: '09:31:40.000', sev: 'INFO', body: 'agent turn started', attrs: { 'gen_ai.request.model': 'claude-haiku-4-5', 'gen_ai.request.max_tokens': 200000 } },
            { t: '09:48:12.400', sev: 'WARN', body: 'context at 92% of window', attrs: { 'gen_ai.usage.input_tokens': 184_000, 'agent.tool_calls': 38 } },
            { t: '09:50:02.900', sev: 'ERROR', body: 'context window exhausted after 41 tool calls', attrs: { 'error.type': 'context_exhausted', 'agent.tool_calls': 41 } },
          ],
        }],
      },
      {
        runId: 'run_e77b02', traceId: 'e77b02f4d9a1483cb5620e7c1a3f95dd', ownerDisplayName: 'Support escalations', status: 'failed', phase: 'provisioning',
        issueId: 'SUP-201', issueTitle: 'Customer escalation: sandbox quota',
        workflowName: 'webhook-escalation', workspaceName: 'support', factoryName: 'release',
        model: 'claude-opus-4-1', repo: 'elasticclaw/elasticclaw',
        cost: 4.60, inputTokens: 148_900, outputTokens: 21_100, totalTokens: 170_000,
        duration: '11m', started: '8/14/2026', startedAt: 'Aug 14, 12:02',
        warningTypes: ['provider_degraded'], failureType: 'sandbox_provision_failed', humanInteractionCount: 0,
        prCount: 0, mergedPrCount: 0,
        timing: [
          { label: 'Queued wait', value: '11m', title: 'Time between the run being queued and the agent starting to work (covers provisioning and startup).' },
        ],
        prs: [],
        attempts: [{ id: 'a14', attemptNumber: 1, status: 'failed', duration: '11m', failureType: 'sandbox_provision_failed' }],
        events: [
          { id: 'e42', eventType: 'run_queued', time: 'Aug 14, 12:02', actor: 'webhook', source: 'webhook' },
          { id: 'e43', eventType: 'provision_failed', time: 'Aug 14, 12:13', actor: 'daytona', source: 'provider' },
        ],
        activity: [],
        outputs: [{
          stage: 'provision', outputName: 'daytona create', attemptId: 'attempt_1', exitCode: 1,
          spanId: '18c4b9e07fa2d536', spanKind: 'CLIENT', durationMs: 11040, status: 'ERROR',
          stdout: '', stderr: 'error: quota exceeded for workspace pool "default" (limit 12)',
          records: [
            { t: '12:02:44.000', sev: 'INFO', body: 'requesting sandbox', attrs: { 'cloud.provider': 'daytona', 'sandbox.pool': 'default' } },
            { t: '12:13:24.040', sev: 'ERROR', body: 'quota exceeded for workspace pool "default" (limit 12)', attrs: { 'error.type': 'quota_exceeded', 'sandbox.pool.limit': 12, 'process.exit_code': 1 } },
          ],
        }],
      },
    ],
    drivers: [
      { name: 'linear-story', runs: 148, successRate: 0.824, costUsd: 612.40, costPerMergedPr: 6.12 },
      { name: 'dependency-updates', runs: 86, successRate: 0.907, costUsd: 284.10, costPerMergedPr: 3.64 },
      { name: 'github-issue', runs: 54, successRate: 0.741, costUsd: 231.80, costPerMergedPr: 8.92 },
      { name: 'release-followup', runs: 30, successRate: 0.667, costUsd: 155.70, costPerMergedPr: 11.05 },
    ],
  }

  /* Ticket-level business metadata that no run carries: who asked, why, when it
     was reported. Everything else on a ticket is derived from its runs. */
  const TICKET_META = {
    'ADV-812': { requester: 'Priya Raman', requesterRole: 'QA lead', team: 'Adversary Labs', source: 'linear', priority: 'High', reportedAt: '2026-08-12T14:20:00Z', ask: 'The hub retry e2e shard fails about one run in fifteen, so release gating is unreliable.', outcome: 'Backoff accounting fixed; shard green over 50 iterations.' },
    'DOC-9': { requester: 'Marc Oliveira', requesterRole: 'Docs owner', team: 'Developer Experience', source: 'github', priority: 'Medium', reportedAt: '2026-08-15T07:40:00Z', ask: 'The install page still documents the pre-1.0 flow and is the top exit page in docs.', outcome: 'Page rewritten against the current CLI; reviewed and merged same day.' },
    'SUP-201': { requester: 'Dana Whitfield', requesterRole: 'Support engineer', team: 'Customer Support', source: 'webhook', priority: 'Urgent', reportedAt: '2026-08-14T11:50:00Z', ask: 'A customer on the growth plan cannot start runs — sandbox quota appears exhausted.', outcome: 'No code change delivered: both runs died provisioning. Needs a human.' },
    'INF-44': { requester: 'Automated', requesterRole: 'Scheduled policy', team: 'Platform', source: 'schedule', priority: 'Low', reportedAt: '2026-08-14T05:30:00Z', ask: 'Weekly dependency bump wave across the Go and web workspaces.', outcome: 'Go module bumps merged; the web bump was parked pending a breaking change.' },
    'REL-7': { requester: 'Automated', requesterRole: 'Release pipeline', team: 'Platform', source: 'schedule', priority: 'Low', reportedAt: '2026-08-13T18:00:00Z', ask: 'Assemble the changelog for the 1.7.0 release from merged PR titles.', outcome: 'Changelog merged with no human interaction.' },
    'ADV-806': { requester: 'Priya Raman', requesterRole: 'QA lead', team: 'Adversary Labs', source: 'linear', priority: 'Medium', reportedAt: '2026-08-13T11:10:00Z', ask: 'Dragging to reorder the sidebar drops pinned agents out of the pinned group.', outcome: null },
    'PLT-31': { requester: 'Sofia Braga', requesterRole: 'Product manager', team: 'Platform', source: 'linear', priority: 'High', reportedAt: '2026-08-14T09:05:00Z', ask: 'Public API consumers cannot see their remaining quota — expose rate limit headers.', outcome: null },
  }

  /* Business phrasing for the ticket story. Run-level noise (attempt_retried,
     stage transitions) is collapsed rather than shown. */
  const STORY_LABEL = {
    run_queued: 'Work queued',
    agent_started: 'Agent started working',
    pr_opened: 'Pull request opened',
    ci_succeeded: 'Checks passed',
    review_commented: 'Human reviewed',
    dashboard_message_sent: 'Human stepped in',
    pr_merged: 'Shipped',
    pr_closed: 'Attempt discarded',
    provision_failed: 'Blocked: no sandbox available',
    verification_failed: 'Blocked: tests would not pass',
    context_exhausted: 'Blocked: agent ran out of context',
    attempt_retried: 'Retried',
  }
  const STORY_KIND = {
    pr_merged: 'good', ci_succeeded: 'good',
    provision_failed: 'bad', verification_failed: 'bad', context_exhausted: 'bad', pr_closed: 'bad',
    review_commented: 'human', dashboard_message_sent: 'human',
  }

  const MONTHS = { Jan: 0, Feb: 1, Mar: 2, Apr: 3, May: 4, Jun: 5, Jul: 6, Aug: 7, Sep: 8, Oct: 9, Nov: 10, Dec: 11 }
  // "Aug 15, 09:43" -> Date (fixtures are all 2026, UTC).
  const parseEventTime = (t) => {
    const m = /^(\w{3}) (\d+), (\d+):(\d+)$/.exec(t || '')
    return m ? new Date(Date.UTC(2026, MONTHS[m[1]], +m[2], +m[3], +m[4])) : null
  }
  const humanizeDuration = (ms) => {
    if (ms == null) return null
    const min = Math.round(ms / 60000)
    if (min < 60) return min + 'm'
    const h = Math.floor(min / 60)
    if (h < 24) return h + 'h ' + String(min % 60).padStart(2, '0') + 'm'
    const d = Math.floor(h / 24)
    return d + 'd ' + (h % 24) + 'h'
  }

  /* The single rule the ticket board turns on. PR_OPEN is what the run-level
     view could never express: work landed, review has not. */
  const deriveTicketStatus = (runs) => {
    const prs = runs.flatMap((r) => r.prs || [])
    if (prs.some((p) => p.state === 'merged')) return 'delivered'
    if (prs.some((p) => p.state === 'open')) return 'pr_open'
    if (runs.some((r) => r.status === 'running')) return 'in_progress'
    if (runs.length && runs.every((r) => r.status === 'failed')) return 'failed'
    return 'in_progress'
  }

  const runsByTicket = window.EC_ANALYTICS.runsTable.reduce((acc, r) => {
    (acc[r.issueId] = acc[r.issueId] || []).push(r)
    return acc
  }, {})

  window.EC_ANALYTICS.tickets = Object.entries(runsByTicket).map(([issueId, all]) => {
    const runs = [...all].sort((a, b) => parseEventTime(a.startedAt) - parseEventTime(b.startedAt))
    const meta = TICKET_META[issueId] || {}
    const status = deriveTicketStatus(runs)
    const prs = runs.flatMap((r) => (r.prs || []).map((p) => ({ ...p, runId: r.runId })))
    const events = runs.flatMap((r) => (r.events || []).map((e) => ({ ...e, runId: r.runId, at: parseEventTime(e.time) })))
      .sort((a, b) => a.at - b.at)

    const reportedAt = meta.reportedAt ? new Date(meta.reportedAt) : null
    const firstStart = parseEventTime(runs[0].startedAt)
    const merged = events.find((e) => e.eventType === 'pr_merged')
    const lastEvent = events[events.length - 1]
    const end = merged ? merged.at : lastEvent && lastEvent.at

    // One story line per business-relevant event; consecutive retries collapse.
    const story = []
    for (const e of events) {
      const label = STORY_LABEL[e.eventType]
      if (!label) continue
      const prev = story[story.length - 1]
      if (e.eventType === 'attempt_retried' && prev && prev.eventType === 'attempt_retried') { prev.count += 1; continue }
      story.push({ id: e.id, eventType: e.eventType, label, actor: e.actor, time: e.time, runId: e.runId, kind: STORY_KIND[e.eventType] || 'neutral', count: 1 })
    }

    return {
      issueId, issueTitle: runs[0].issueTitle, status, ...meta,
      repo: runs[0].repo, workflowName: runs[0].workflowName, workspaceName: runs[0].workspaceName,
      runIds: runs.map((r) => r.runId), runCount: runs.length,
      attemptCount: runs.reduce((s, r) => s + (r.attempts || []).length, 0),
      failedRunCount: runs.filter((r) => r.status === 'failed').length,
      cost: runs.reduce((s, r) => s + r.cost, 0),
      totalTokens: runs.reduce((s, r) => s + r.totalTokens, 0),
      humanTouches: runs.reduce((s, r) => s + r.humanInteractionCount, 0),
      prs, mergedPrCount: prs.filter((p) => p.state === 'merged').length,
      openPrCount: prs.filter((p) => p.state === 'open').length,
      reportedAtLabel: reportedAt && reportedAt.toLocaleString('en', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false, timeZone: 'UTC' }),
      timeToFirstRun: reportedAt && firstStart ? humanizeDuration(firstStart - reportedAt) : null,
      leadTime: reportedAt && end ? humanizeDuration(end - reportedAt) : null,
      deliveredAtLabel: merged ? merged.time : null,
      lastActivity: lastEvent ? lastEvent.time : runs[runs.length - 1].startedAt,
      story,
    }
  }).sort((a, b) => b.cost - a.cost)
})();
