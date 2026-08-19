'use client'

import * as React from 'react'
import { Copy, ExternalLink } from 'lucide-react'
import type { DateRange } from 'react-day-picker'
import { notFound } from 'next/navigation'

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

export default function DesignSystemGalleryPage() {
  const [range, setRange] = React.useState<DateRange | undefined>()
  if (process.env.NODE_ENV !== 'development') notFound()

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
    </main>
  )
}
