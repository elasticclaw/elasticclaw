import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'

import { cn } from '@/lib/utils'

// Industry buttons are square, heading-font and hairline-bordered. The solid
// accent `default` is the one filled object on the board; everything else is a
// line drawing. Hover/pressed tints come from the accent ramp, never opacity.
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap font-heading text-sm font-semibold transition-colors disabled:pointer-events-none disabled:opacity-45 [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-4 shrink-0 [&_svg]:shrink-0 outline-none aria-invalid:border-destructive",
  {
    variants: {
      variant: {
        default:
          'bg-primary text-primary-foreground border border-primary hover:bg-industry-400 active:bg-industry-300',
        destructive:
          'bg-destructive text-destructive-foreground border border-destructive hover:bg-destructive/85 active:bg-destructive/75',
        outline:
          'border border-border bg-transparent text-foreground hover:bg-foreground/7 active:bg-foreground/14',
        secondary:
          'border border-border bg-transparent text-secondary-foreground hover:bg-foreground/7 active:bg-foreground/14',
        ghost:
          'border border-transparent text-accent-foreground hover:bg-primary/10 active:bg-primary/18',
        link: 'text-accent-foreground underline-offset-4 hover:underline',
      },
      size: {
        default: 'h-9 px-4 py-2 has-[>svg]:px-3',
        sm: 'h-8 gap-1.5 px-3 has-[>svg]:px-2.5',
        lg: 'h-10 px-6 has-[>svg]:px-4',
        icon: 'size-9',
        'icon-sm': 'size-8',
        'icon-lg': 'size-10',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
)

function Button({
  className,
  variant,
  size,
  asChild = false,
  ...props
}: React.ComponentProps<'button'> &
  VariantProps<typeof buttonVariants> & {
    asChild?: boolean
  }) {
  const Comp = asChild ? Slot : 'button'

  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button, buttonVariants }
