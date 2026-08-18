import { ArrowDown, ArrowUp } from 'lucide-react'
import type * as React from 'react'

import { cn } from '@/lib/utils'

export interface KpiTileProps extends React.ComponentProps<'div'> {
  label: string
  value: React.ReactNode
  delta?: React.ReactNode
  deltaDirection?: 'up' | 'down'
  deltaTone?: 'good' | 'bad'
}

export function KpiTile({ label, value, delta, deltaDirection = 'up', deltaTone = 'good', className, ...props }: KpiTileProps) {
  const Arrow = deltaDirection === 'up' ? ArrowUp : ArrowDown
  const color = deltaTone === 'good' ? 'text-chart-2' : 'text-chart-4'
  return (
    <div className={cn('min-w-[158px] border p-3', className)} {...props}>
      <div className="text-[10px] font-medium uppercase tracking-[0.08em] text-muted-foreground">{label}</div>
      <div className="mt-1 font-mono text-xl tabular-nums">{value}</div>
      {delta ? <div className={cn('mt-1 flex items-center gap-1 font-mono text-[11px]', color)}><Arrow className="size-3" />{delta}</div> : null}
    </div>
  )
}
