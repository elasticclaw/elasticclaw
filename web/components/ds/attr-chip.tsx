import { cn } from '@/lib/utils'
import type * as React from 'react'

export interface AttrChipProps extends React.ComponentProps<'span'> { k: string; v: string | number | boolean | null | undefined }

export function AttrChip({ k, v, className, ...props }: AttrChipProps) {
  const numeric = typeof v === 'number'
  const value = String(v)
  const displayValue = k === 'process.command' && value.length > 500 ? `${value.slice(0, 500)}…` : value
  return <span className={cn('inline-flex items-baseline gap-0.5 rounded-sm bg-muted px-1 font-mono text-[10px]', className)} {...props}><span className="text-muted-foreground">{k}</span><span className="text-muted-foreground">=</span><span className={numeric ? 'text-chart-3' : 'text-foreground'}>{displayValue}</span></span>
}
