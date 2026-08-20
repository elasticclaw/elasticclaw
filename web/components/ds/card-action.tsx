import { Check } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import * as React from 'react'

import { cn } from '@/lib/utils'

export interface CardActionProps extends Omit<React.ComponentProps<'button'>, 'children'> {
  icon: LucideIcon
  label: string
  count?: number
  confirmed?: boolean
  tone?: string
}

export function CardAction({ icon: Icon, label, count, confirmed = false, tone, className, onMouseEnter, onMouseLeave, ...props }: CardActionProps) {
  const [hover, setHover] = React.useState(false)
  const hasCount = (count ?? 0) > 0
  const color = confirmed ? 'var(--chart-2)' : tone ?? (hover ? 'var(--foreground)' : 'var(--muted-foreground)')

  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onMouseEnter={(event) => {
        setHover(true)
        onMouseEnter?.(event)
      }}
      onMouseLeave={(event) => {
        setHover(false)
        onMouseLeave?.(event)
      }}
      className={cn('inline-flex h-6 shrink-0 items-center gap-1 rounded-md border bg-transparent px-[5px] font-mono text-[10px] font-medium transition-colors hover:bg-accent', hasCount && 'pr-[7px]', className)}
      style={{ borderColor: hasCount ? 'var(--border)' : 'transparent', color }}
      {...props}
    >
      {confirmed ? <Check className="size-3.5" /> : <Icon className="size-3.5" />}
      {hasCount ? <span>{count}</span> : null}
    </button>
  )
}
