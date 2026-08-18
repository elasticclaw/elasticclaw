import { Check } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import type * as React from 'react'

import { cn } from '@/lib/utils'

export interface CardActionProps extends Omit<React.ComponentProps<'button'>, 'children'> {
  icon: LucideIcon
  label: string
  count?: number
  confirmed?: boolean
}

export function CardAction({ icon: Icon, label, count, confirmed = false, className, ...props }: CardActionProps) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      className={cn('relative inline-flex size-7 items-center justify-center border-l text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground', className)}
      {...props}
    >
      {confirmed ? <Check className="size-4 text-chart-2" /> : <Icon className="size-4" />}
      {count !== undefined ? <span className="absolute -right-1 -top-1 min-w-4 rounded-full bg-chart-1 px-1 text-[10px] leading-4 text-white">{count}</span> : null}
    </button>
  )
}
