import { Info } from 'lucide-react'
import type * as React from 'react'

import { cn } from '@/lib/utils'

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
      <header className="flex min-h-9 items-center justify-between border-b px-3 text-xs font-medium">
        <span>{title}</span>
        {headerAction ?? (info ? <span title={info} aria-label={info}><Info className="size-3.5 text-muted-foreground" /></span> : null)}
      </header>
      <div className={cn('min-h-0 flex-1 p-3', bodyClassName)}>{children}</div>
      {stat ? <footer className="flex items-center gap-3 border-t px-3 py-1.5 font-mono text-[10px] text-muted-foreground">{stat}</footer> : null}
    </section>
  )
}

export const ChartCard = Panel
