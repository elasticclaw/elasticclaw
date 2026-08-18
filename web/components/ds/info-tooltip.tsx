'use client'

import { Info } from 'lucide-react'

import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

export interface InfoTooltipProps {
  content: string
  className?: string
  iconClassName?: string
}

/* The affordance-right info glyph of the card anatomy. Native `title` needs a
   long hover and renders in the OS chrome; this opens instantly, keyboard
   included, styled like the heatmap's tooltip (card surface, border, shadow). */
export function InfoTooltip({ content, className, iconClassName }: InfoTooltipProps) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span tabIndex={0} aria-label={content} className={cn('flex shrink-0 cursor-help text-muted-foreground transition-colors hover:text-foreground focus-visible:text-foreground focus-visible:outline-none', className)}>
          <Info className={cn('size-3.5', iconClassName)} />
        </span>
      </TooltipTrigger>
      <TooltipContent sideOffset={6} className="max-w-xs rounded-lg border bg-card px-3 py-2 text-xs leading-relaxed text-foreground shadow-md" arrowClassName="bg-card fill-card">
        {content}
      </TooltipContent>
    </Tooltip>
  )
}
