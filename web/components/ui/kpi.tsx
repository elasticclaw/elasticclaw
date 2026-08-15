import * as React from 'react'

import { cn } from '@/lib/utils'

/**
 * KpiGrid — one hairline-bordered grid of KPI cells with 1px internal
 * dividers. The dividers are the grid's own gap showing through: the grid
 * background is the border color and each cell paints the ground back over it,
 * so there are no double rules and no per-cell border bookkeeping.
 *
 * Shape the grid with grid-cols-* classes (responsive variants included), or
 * pass `columns` for a fixed count — the inline style is only emitted when
 * `columns` is given, so it never overrides responsive classes.
 */
function KpiGrid({
  className,
  columns,
  style,
  ...props
}: React.ComponentProps<'div'> & { columns?: number }) {
  return (
    <div
      data-slot="kpi-grid"
      className={cn('border-border bg-border grid gap-px border', className)}
      style={
        columns !== undefined
          ? { gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))`, ...style }
          : style
      }
      {...props}
    />
  )
}

/**
 * KpiCell — kicker caption, a 30px condensed tabular number, and an optional
 * delta line. `deltaTone` picks the delta color: muted by default, accent when
 * the movement is the point.
 */
function KpiCell({
  label,
  value,
  delta,
  deltaTone = 'muted',
  className,
  children,
  ...props
}: Omit<React.ComponentProps<'div'>, 'children'> & {
  label: React.ReactNode
  value: React.ReactNode
  delta?: React.ReactNode
  deltaTone?: 'muted' | 'accent'
  children?: React.ReactNode
}) {
  return (
    <div
      data-slot="kpi-cell"
      className={cn('bg-background flex flex-col gap-1 p-4', className)}
      {...props}
    >
      <span className="kicker">{label}</span>
      <span className="font-heading text-[30px] leading-none font-semibold tabular-nums">
        {value}
      </span>
      {delta ? (
        <span
          className={cn(
            'text-[11px]',
            deltaTone === 'accent'
              ? 'text-accent-foreground'
              : 'text-muted-foreground',
          )}
        >
          {delta}
        </span>
      ) : null}
      {children}
    </div>
  )
}

export { KpiGrid, KpiCell }
