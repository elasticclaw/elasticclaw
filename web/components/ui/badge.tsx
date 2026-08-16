import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'

import { cn } from '@/lib/utils'

// Modernist tags: pill-shaped, 11px, and tinted rather than filled — the solid
// accent stays reserved for buttons and live status marks.
const badgeVariants = cva(
  'inline-flex items-center justify-center rounded-full border border-transparent px-2.5 py-0.5 text-[11px] leading-4 font-medium tracking-[0.02em] w-fit whitespace-nowrap shrink-0 [&>svg]:size-3 gap-1 [&>svg]:pointer-events-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring aria-invalid:border-destructive transition-colors overflow-hidden',
  {
    variants: {
      variant: {
        default:
          'bg-tint-accent text-tint-accent-foreground [a&]:hover:bg-tint-accent-2',
        secondary:
          'bg-tint-neutral text-tint-neutral-foreground [a&]:hover:bg-foreground/16',
        destructive:
          'bg-destructive text-primary-foreground [a&]:hover:bg-[var(--ds-accent-hover)]',
        outline:
          'border-primary text-primary [a&]:hover:bg-tint-accent [a&]:hover:text-tint-accent-foreground',
      },
    },
    defaultVariants: {
      variant: 'default',
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
