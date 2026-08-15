import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'

import { cn } from '@/lib/utils'

/**
 * Kicker — the 10px uppercase tracked label that sits above a heading, a KPI
 * value or a table section. Muted by default; pass `emphasis` for the accent
 * variant used when the label itself carries meaning (e.g. a live state).
 */
function Kicker({
  className,
  emphasis = false,
  asChild = false,
  ...props
}: React.ComponentProps<'span'> & { emphasis?: boolean; asChild?: boolean }) {
  const Comp = asChild ? Slot : 'span'

  return (
    <Comp
      data-slot="kicker"
      className={cn('kicker', emphasis && 'text-accent-foreground', className)}
      {...props}
    />
  )
}

export { Kicker }
