'use client'

import * as React from 'react'
import { Copy, ExternalLink } from 'lucide-react'
import type { DateRange } from 'react-day-picker'

import { SubagentRail } from '@/components/agent-timeline/subagent-rail'
import { SubagentLanes } from '@/components/agent-timeline/subagent-lanes'
import { SubagentDetail } from '@/components/agent-timeline/subagent-detail'
import { currentTurnSubagents, type Subagent } from '@/lib/subagents'
import type { AgentActivity, Message } from '@/lib/types'
import {
  AgentStateChip,
  AGENT_SECTION,
  AttrChip,
  CardAction,
  ChartCard,
  DatePickerRange,
  KpiTile,
  RunStatusBadge,
  SeverityChip,
  TicketStatusBadge,
  agentSection,
  personColor,
} from '@/components/ds'

function GalleryItem({ label, children }: { label: string; children: React.ReactNode }) {
  return <div className="flex flex-wrap items-center gap-3 border-b py-4"><span className="w-72 font-mono text-xs text-muted-foreground">{label}</span>{children}</div>
}


// ─── Subagent fixtures ───────────────────────────────────────────────────────
// Built as raw messages and run through the real derivation
// (currentTurnSubagents) rather than hand-authored Subagent objects: a fixture
// that skips the pipeline stops catching regressions in it. The clock is fixed
// so the four states are stable on every render, including on the server.

const FIXTURE_NOW = Date.UTC(2026, 7, 24, 15, 0, 0)
const at = (secondsAgo: number) => new Date(FIXTURE_NOW - secondsAgo * 1000)

let fixtureSeq = 0
function activityMessage(timestamp: Date, activity: AgentActivity): Message {
  fixtureSeq += 1
  return { id: `fixture-${fixtureSeq}`, role: 'activity', content: '', timestamp, activity }
}

function taskStart(
  callId: string,
  secondsAgo: number,
  fields: Partial<AgentActivity> & { subagent_name: string; subagent_prompt: string },
): Message {
  return activityMessage(at(secondsAgo), {
    kind: 'tool',
    tool: 'Task',
    phase: 'running',
    call_id: callId,
    subagent_model: 'claude-opus-5',
    ...fields,
  })
}

function taskEnd(callId: string, secondsAgo: number, fields: Partial<AgentActivity>): Message {
  return activityMessage(at(secondsAgo), { kind: 'tool', tool: 'Task', phase: 'completed', call_id: callId, ...fields })
}

const LONG_NAME =
  'refactor-the-entire-billing-subsystem-and-its-migrations-worker-pool-agent'

const SUBAGENT_FIXTURE_MESSAGES: Message[] = [
  { id: 'fixture-user', role: 'user', content: 'Ship the subagent rail.', timestamp: at(900) },

  // running — output seconds ago, well inside the staleness threshold
  taskStart('call-running', 420, {
    subagent_name: 'web-implementation',
    subagent_type: 'general-purpose',
    subagent_prompt:
      'Build the right-hand subagent rail, the lane strip and the drill-down, matching the approved AgentConsole mock and the existing timeline conventions.',
    detail: 'components/agent-timeline/subagent-rail.tsx',
    path: 'components/agent-timeline/subagent-rail.tsx',
  }),
  activityMessage(at(4), {
    kind: 'tool',
    tool: 'Task',
    phase: 'running',
    call_id: 'call-running',
    path: 'web/components/agent-timeline/subagent-rail.tsx',
  }),

  // running — a name long enough to prove the truncation
  taskStart('call-running-long', 300, {
    subagent_name: LONG_NAME,
    subagent_type: 'general-purpose',
    subagent_prompt:
      'Sweep every call site of the legacy billing adapter, port it to the new ledger API, and regenerate the migrations that depend on the old column names.',
    command: 'rg -n "legacyBillingAdapter" --glob "!node_modules" -S',
  }),
  activityMessage(at(11), {
    kind: 'tool',
    tool: 'Task',
    phase: 'running',
    call_id: 'call-running-long',
    command: 'go test ./internal/billing/... -run TestLedgerMigration -count=1',
  }),

  // quiet — last output well past SUBAGENT_STALE_MS (30s)
  taskStart('call-quiet', 660, {
    subagent_name: 'depot-ci-investigator',
    subagent_type: 'general-purpose',
    subagent_prompt:
      'Find out why the integration job flakes on the gateway port bind and report whether it is a runner flake or a real race.',
    command: 'depot logs --build 4f21ac --follow',
  }),
  activityMessage(at(214), {
    kind: 'tool',
    tool: 'Task',
    phase: 'running',
    call_id: 'call-quiet',
    command: 'depot logs --build 4f21ac --follow',
  }),

  // failed
  taskStart('call-failed', 540, {
    subagent_name: 'android-gate-validator',
    subagent_type: 'general-purpose',
    subagent_prompt: 'Run the android_validation gate against the current worktree and report the first failing check.',
  }),
  taskEnd('call-failed', 468, {
    phase: 'failed',
    exit_code: 1,
    error:
      'android_validation: scripts/ was never mounted into the sandbox\n  at gate.Run (internal/gates/android.go:88)\n  the gate has no error branch, so the agent hard-stops here',
  }),

  // done
  taskStart('call-done', 780, {
    subagent_name: 'bridge-probe',
    subagent_type: 'general-purpose',
    subagent_prompt: 'Probe the gateway for nested-session and Task tool payloads and document the field names it emits.',
  }),
  taskEnd('call-done', 690, {
    phase: 'completed',
    result:
      'Gateway emits subagent_name, subagent_type, subagent_model and subagent_prompt on the Task tool start event. Nested session ids are not forwarded.',
  }),
]

const SUBAGENT_FIXTURES: Subagent[] =
  currentTurnSubagents(SUBAGENT_FIXTURE_MESSAGES, FIXTURE_NOW) ?? []

/** 50+ cards: the rail must stay scrollable and readable, not collapse. */
const MANY_SUBAGENT_FIXTURES: Subagent[] =
  SUBAGENT_FIXTURES.length === 0
    ? []
    : Array.from({ length: 56 }, (_, i) => {
        const base = SUBAGENT_FIXTURES[i % SUBAGENT_FIXTURES.length]
        return { ...base, id: `${base.id}-clone-${i}`, name: `${base.name}-${i + 1}` }
      })

function byStatus(status: Subagent['status']): Subagent | undefined {
  return SUBAGENT_FIXTURES.find((sub) => sub.status === status)
}

function SubagentGallery() {
  return (
    <section className="mt-8">
      <h2 className="text-sm font-medium">Subagents</h2>
      <p className="mt-1 text-xs text-muted-foreground">
        Derived from fixture messages through the real pipeline, against a frozen clock. States
        present: {SUBAGENT_FIXTURES.map((sub) => sub.status).join(', ') || 'none — the fixture pipeline returned nothing'}.
      </p>

      <GalleryItem label="SubagentLanes — all four states">
        <div className="min-w-0 flex-1 rounded-lg border">
          <SubagentLanes subagents={SUBAGENT_FIXTURES} now={FIXTURE_NOW} onOpen={() => {}} />
        </div>
      </GalleryItem>

      <GalleryItem label="SubagentRail — active above, finished below">
        <div className="flex h-[420px] min-w-0 flex-1 justify-end rounded-lg border">
          <SubagentRail subagents={SUBAGENT_FIXTURES} now={FIXTURE_NOW} onOpen={() => {}} />
        </div>
      </GalleryItem>

      <GalleryItem label="SubagentRail — 56 subagents (scroll load)">
        <div className="flex h-[420px] min-w-0 flex-1 justify-end rounded-lg border">
          <SubagentRail subagents={MANY_SUBAGENT_FIXTURES} now={FIXTURE_NOW} onOpen={() => {}} />
        </div>
      </GalleryItem>

      <GalleryItem label="SubagentRail — empty">
        <div className="flex h-40 min-w-0 flex-1 justify-end rounded-lg border">
          <SubagentRail subagents={[]} now={FIXTURE_NOW} onOpen={() => {}} />
        </div>
      </GalleryItem>

      {(['running', 'quiet', 'failed', 'done'] as const).map((status) => {
        const sub = byStatus(status)
        return (
          <GalleryItem key={status} label={`SubagentDetail — ${status}`}>
            <div className="min-w-0 flex-1 rounded-lg border p-4">
              {sub ? (
                <SubagentDetail subagent={sub} now={FIXTURE_NOW} />
              ) : (
                <span className="text-xs text-muted-foreground">no {status} fixture</span>
              )}
            </div>
          </GalleryItem>
        )
      })}
    </section>
  )
}

export function DesignSystemGalleryPage() {
  const [range, setRange] = React.useState<DateRange | undefined>()
  return (
    <main className="mx-auto max-w-5xl p-8">
      <h1 className="text-2xl font-semibold">Design system gallery</h1>
      <section className="mt-6">
        <h2 className="text-sm font-medium">Agent state chips</h2>
        {(['streaming', 'connected', 'provisioning', 'idle', 'offline', 'error'] as const).flatMap((status) => [
          <GalleryItem key={`${status}-sm`} label={`AgentStateChip — status=${status}, size=sm`}><AgentStateChip status={status} /></GalleryItem>,
          <GalleryItem key={`${status}-md`} label={`AgentStateChip — status=${status}, size=md`}><AgentStateChip status={status} size="md" reason="Example reason" /></GalleryItem>,
        ])}
        <GalleryItem label="AgentStateChip — isStreaming overrides status"><AgentStateChip status="idle" isStreaming /></GalleryItem>
      </section>
      <section className="mt-8">
        <h2 className="text-sm font-medium">Date picker</h2>
        <GalleryItem label="DatePickerRange — presets and range"><DatePickerRange value={range} onChange={setRange} /></GalleryItem>
      </section>
      <section className="mt-8">
        <h2 className="text-sm font-medium">Agent sections</h2>
        <GalleryItem label="agentSection — attention / working / offline"><span className="font-mono text-xs">{agentSection({ status: 'idle', unreadCount: 1 }, { isWaitingOnYou: false })} · {agentSection({ status: 'connected', unreadCount: 0 }, { isWaitingOnYou: false })} · {agentSection({ status: 'offline', unreadCount: 0 }, { isWaitingOnYou: false })}</span></GalleryItem>
        {Object.entries(AGENT_SECTION).map(([name, section]) => <GalleryItem key={name} label={`AGENT_SECTION — ${name}`}><section.icon className="size-4" style={{ color: section.color }} /><span className="text-sm">{section.label} — {section.empty}</span></GalleryItem>)}
      </section>
      <section className="mt-8">
        <h2 className="text-sm font-medium">Card actions and KPI tiles</h2>
        <GalleryItem label="CardAction — default / count / confirmed"><div className="flex items-center gap-2"><CardAction icon={ExternalLink} label="Open pull requests" count={2} tone="var(--chart-1)" /><CardAction icon={Copy} label="Copy transcript" confirmed /></div></GalleryItem>
        <GalleryItem label="KpiTile — good and bad deltas"><KpiTile label="Delivered" value="42" delta="12%" deltaDirection="up" deltaTone="good" info="Delivered tickets compared with the prior period." /><KpiTile label="Cost" value="$83.20" delta="8%" deltaDirection="up" deltaTone="bad" /></GalleryItem>
      </section>
      <section className="mt-8">
        <h2 className="text-sm font-medium">Cards, tickets, and log chips</h2>
        <GalleryItem label="ChartCard — header, body, stat line"><ChartCard className="w-80" title="Throughput" info="Completed tickets by day" stat="4 steps · ctx 64%"><div className="text-sm text-muted-foreground">Chart body</div></ChartCard></GalleryItem>
        {(['delivered', 'pr_open', 'in_progress', 'failed'] as const).map((status) => <GalleryItem key={status} label={`TicketStatusBadge — ${status}`}><TicketStatusBadge status={status} /></GalleryItem>)}
        {(['clean', 'human_in_the_loop', 'warning', 'failed', 'running', 'fallback'] as const).map((status) => <GalleryItem key={status} label={`RunStatusBadge — ${status}`}><RunStatusBadge status={status} /></GalleryItem>)}
        {(['TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR', 'FATAL'] as const).map((severity) => <GalleryItem key={severity} label={`SeverityChip — ${severity}`}><SeverityChip severity={severity} /><AttrChip k="duration_ms" v={240} /><AttrChip k="service.name" v="elasticclaw" /></GalleryItem>)}
        <GalleryItem label="personColor — stable chart token"><span className="font-mono text-xs" style={{ color: personColor('anaberg') }}>{personColor('anaberg')}</span></GalleryItem>
      </section>
      <SubagentGallery />
    </main>
  )
}

export default DesignSystemGalleryPage
