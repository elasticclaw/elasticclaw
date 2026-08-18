import type * as React from 'react'

import { cn } from '@/lib/utils'
import { InfoTooltip } from './info-tooltip'

export interface PanelProps extends Omit<React.ComponentProps<'section'>, 'title'> {
  title: React.ReactNode
  info?: string
  stat?: React.ReactNode
  headerAction?: React.ReactNode
  bodyClassName?: string
}

export function Panel({ title, info, stat, headerAction, className, bodyClassName, children, ...props }: PanelProps) {
  return (
    <section className={cn('flex flex-col overflow-hidden rounded-lg border bg-card', className)} {...props}>
      <header className="flex items-center gap-2 border-b p-3">
        <span className="min-w-0 flex-1 truncate text-sm font-medium">{title}</span>
        {headerAction ?? (info ? <InfoTooltip content={info} /> : null)}
      </header>
      <div className={cn('min-h-0 flex-1 p-3', bodyClassName)}>{children}</div>
      {stat ? <footer className="flex items-center gap-3 border-t px-3 py-1 font-mono text-[10px] text-muted-foreground">{stat}</footer> : null}
    </section>
  )
}

export const ChartCard = Panel
