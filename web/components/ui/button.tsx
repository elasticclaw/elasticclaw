import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'

import { cn } from '@/lib/utils'

// Modernist buttons: the heading face at weight 800, 14px, 8px radius. The
// accent carries the primary action; everything else is a divider outline or a
// bare ink wash, so a screen never shows two competing filled buttons.
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-extrabold leading-none tracking-normal transition-colors disabled:pointer-events-none disabled:opacity-45 [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-4 shrink-0 [&_svg]:shrink-0 outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring aria-invalid:border-destructive",
  {
    variants: {
      variant: {
        default:
          'bg-primary text-primary-foreground hover:bg-[var(--ds-accent-hover)] active:bg-[var(--ds-accent-active)]',
        destructive:
          'bg-destructive text-primary-foreground hover:bg-[var(--ds-accent-hover)] active:bg-[var(--ds-accent-active)]',
        outline:
          'border border-border bg-transparent text-foreground hover:bg-foreground/7 active:bg-foreground/14',
        secondary:
          'bg-secondary text-secondary-foreground hover:bg-foreground/14 active:bg-foreground/18',
        ghost: 'hover:bg-foreground/8 active:bg-foreground/14',
        link: 'text-primary underline-offset-4 hover:underline hover:text-[var(--ds-accent-hover)]',
      },
      size: {
        default: 'h-9 px-4 py-2 has-[>svg]:px-3',
        sm: 'h-8 rounded-md gap-1.5 px-3 has-[>svg]:px-2.5',
        lg: 'h-10 rounded-md px-6 has-[>svg]:px-4',
        icon: 'size-9',
        'icon-sm': 'size-8',
        'icon-lg': 'size-10',
      },
    },
    compoundVariants: [
      // A ghost button carrying a label reads as the system's tertiary action,
      // so it takes the accent. Icon-only ghosts stay ink — the shell is full
      // of them and an all-red toolbar would drown the real accents.
      { variant: 'ghost', size: 'default', className: 'text-primary' },
      { variant: 'ghost', size: 'sm', className: 'text-primary' },
      { variant: 'ghost', size: 'lg', className: 'text-primary' },
    ],
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
