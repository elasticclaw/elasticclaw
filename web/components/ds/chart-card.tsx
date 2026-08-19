import * as React from 'react'

import { cn } from '@/lib/utils'
import { InfoTooltip } from './info-tooltip'

export interface PanelProps extends Omit<React.ComponentProps<'section'>, 'title'> {
  title: React.ReactNode
  info?: string
  sub?: React.ReactNode
  stat?: React.ReactNode | { left: React.ReactNode; right?: React.ReactNode }
  headerAction?: React.ReactNode
  extra?: React.ReactNode
  bodyClassName?: string
}

export function Panel({ title, info, sub, stat, headerAction, extra, className, bodyClassName, children, ...props }: PanelProps) {
  const statPair = stat && typeof stat === 'object' && !React.isValidElement(stat) && 'left' in stat ? stat : undefined
  const statTextPair = typeof stat === 'string' && stat.includes(' · ') ? stat.split(' · ', 2) : undefined
  return (
    <section className={cn('flex flex-col overflow-hidden rounded-lg border bg-card', className)} {...props}>
      <header className="flex items-center gap-2 border-b p-3">
        <span className="min-w-0 shrink truncate text-sm font-medium">{title}</span>
        {sub && <span className="min-w-0 truncate text-xs text-muted-foreground">{sub}</span>}
        <span className="ml-auto flex shrink-0 items-center gap-2">{info ? <InfoTooltip content={info} /> : null}{headerAction}{extra}</span>
      </header>
      <div className={cn('min-h-0 flex-1 p-3', bodyClassName)}>{children}</div>
      {stat ? <footer className="flex items-center gap-3 border-t px-3 py-1 font-mono text-[10px] text-muted-foreground">{statPair ? <><span>{statPair.left}</span>{statPair.right && <span className="ml-auto">{statPair.right}</span>}</> : statTextPair ? <><span>{statTextPair[0]}</span><span className="ml-auto">{statTextPair[1]}</span></> : stat as React.ReactNode}</footer> : null}
    </section>
  )
}

export const ChartCard = Panel
