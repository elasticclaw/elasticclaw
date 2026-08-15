import * as React from 'react'

import { cn } from '@/lib/utils'

function Textarea({ className, ...props }: React.ComponentProps<'textarea'>) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        'border-border bg-surface caret-primary placeholder:text-muted-foreground hover:border-foreground/45 focus-visible:border-ring focus-visible:outline-none aria-invalid:border-destructive flex field-sizing-content min-h-16 w-full border px-3 py-2 text-base transition-colors outline-none disabled:cursor-not-allowed disabled:opacity-45 md:text-sm',
        className,
      )}
      {...props}
    />
  )
}

export { Textarea }
