import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'

import { cn } from '@/lib/utils'

// Industry tags: 11px, square, tinted from the ramps. `accent` and `neutral`
// are the canonical names; `default` and `secondary` are kept as aliases so
// existing call sites keep working.
const badgeVariants = cva(
  'inline-flex items-center justify-center border px-2.5 py-0.5 text-[11px] leading-tight font-medium w-fit whitespace-nowrap shrink-0 [&>svg]:size-3 gap-1 [&>svg]:pointer-events-none aria-invalid:border-destructive transition-colors overflow-hidden',
  {
    variants: {
      variant: {
        accent:
          'border-transparent bg-industry-100 text-industry-800 [a&]:hover:bg-industry-200',
        neutral:
          'border-transparent bg-neutral-100 text-neutral-800 [a&]:hover:bg-neutral-200',
        outline:
          'border-primary text-accent-foreground [a&]:hover:bg-primary/10',
        destructive:
          'border-transparent bg-destructive text-destructive-foreground [a&]:hover:bg-destructive/85',
        /** @deprecated alias of `accent` */
        default:
          'border-transparent bg-industry-100 text-industry-800 [a&]:hover:bg-industry-200',
        /** @deprecated alias of `neutral` */
        secondary:
          'border-transparent bg-neutral-100 text-neutral-800 [a&]:hover:bg-neutral-200',
      },
    },
    defaultVariants: {
      variant: 'accent',
    },
  },
)

function Badge({
  className,
  variant,
  asChild = false,
  ...props
}: React.ComponentProps<'span'> &
  VariantProps<typeof badgeVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : 'span'

  return (
    <Comp
      data-slot="badge"
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  )
}

export { Badge, badgeVariants }
