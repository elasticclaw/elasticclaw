import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'

import { cn } from '@/lib/utils'

/**
 * Blueprint — the frame every panel, card, chart and form section wears in the
 * Industry design system: a transparent box with a hairline border and four
 * 11px "+" registration marks sitting 6px OUTSIDE its corners.
 *
 * The marks are real child spans rather than pseudo-elements so a consumer can
 * still use ::before / ::after on the frame. Because they live outside the box,
 * the frame itself must never be `overflow-hidden` — put scroll containers in
 * a child instead.
 */
function Blueprint({
  className,
  children,
  asChild = false,
  ...props
}: React.ComponentProps<'div'> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : 'div'

  return (
    <Comp data-slot="blueprint" className={cn('blueprint', className)} {...props}>
      <span aria-hidden className="corner tl" />
      <span aria-hidden className="corner tr" />
      <span aria-hidden className="corner bl" />
      <span aria-hidden className="corner br" />
      {children}
    </Comp>
  )
}

export { Blueprint }
