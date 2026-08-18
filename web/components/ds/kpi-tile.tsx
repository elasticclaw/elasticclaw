import { ArrowDownRight, ArrowUpRight, Info } from 'lucide-react'
import type * as React from 'react'

import { cn } from '@/lib/utils'

export interface KpiTileProps extends React.ComponentProps<'div'> {
  label: string
  value: React.ReactNode
  delta?: React.ReactNode
  deltaDirection?: 'up' | 'down'
  deltaTone?: 'good' | 'bad'
  info?: string
  tooltip?: string
}

export function KpiTile({ label, value, delta, deltaDirection = 'up', deltaTone = 'good', info, tooltip, className, ...props }: KpiTileProps) {
  const Arrow = deltaDirection === 'up' ? ArrowUpRight : ArrowDownRight
  const color = deltaTone === 'good' ? 'text-chart-2' : 'text-chart-4'
  const help = info ?? tooltip

  return (
    <div className={cn('flex min-w-[158px] flex-col gap-1 rounded-lg border bg-card p-[10px_12px] text-left transition-colors hover:border-ring', className)} {...props}>
      <div className="flex min-w-0 items-start gap-1 text-[10px] font-medium uppercase leading-[1.3] tracking-[0.08em] text-muted-foreground">
        <span className="min-w-0 [overflow-wrap:anywhere]">{label}</span>
        {help ? <span title={help} className="flex shrink-0 cursor-help"><Info className="size-3" /></span> : null}
      </div>
      <div className="mt-auto flex flex-col gap-px">
        <div className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap font-mono text-lg font-medium tracking-tight tabular-nums">{value ?? '—'}</div>
        <div className={cn('flex min-w-0 items-center gap-[3px] overflow-hidden whitespace-nowrap font-mono text-[11px]', color, !delta && 'invisible')}><Arrow className="size-3" />{delta ?? '—'}<span className="min-w-0 overflow-hidden text-ellipsis text-muted-foreground">vs prior</span></div>
      </div>
    </div>
  )
}
