import * as React from 'react'

import { cn } from '@/lib/utils'

type Status = 'active' | 'idle' | 'offline' | 'warning' | 'failed'

const statusColor: Record<Status, string> = {
  active: 'bg-status-active',
  idle: 'bg-status-idle',
  offline: 'bg-status-offline',
  warning: 'bg-status-warning',
  failed: 'bg-status-failed',
}

const statusLabel: Record<Status, string> = {
  active: 'Active',
  idle: 'Idle',
  offline: 'Offline',
  warning: 'Warning',
  failed: 'Failed',
}

/**
 * StatusDot — a 7px round dot. Dots and avatars are the only round objects in
 * the Industry system; everything else is square. Colors come from the status
 * tokens, never a raw palette class.
 *
 * `pulse` animates the dot for live/streaming states (only meaningful on
 * "active").
 */
function StatusDot({
  status,
  pulse = false,
  label,
  className,
  ...props
}: React.ComponentProps<'span'> & {
  status: Status
  pulse?: boolean
  /** Accessible name; defaults to the status word. Pass null to hide it. */
  label?: string | null
}) {
  const accessibleName = label === undefined ? statusLabel[status] : label

  return (
    <span
      data-slot="status-dot"
      data-status={status}
      role={accessibleName ? 'img' : undefined}
      aria-label={accessibleName ?? undefined}
      aria-hidden={accessibleName ? undefined : true}
      className={cn(
        'inline-block size-[7px] shrink-0 rounded-full',
        statusColor[status],
        pulse && 'animate-pulse',
        className,
      )}
      {...props}
    />
  )
}

/**
 * UnreadBadge — a small SQUARE accent-filled count. The square is deliberate:
 * it distinguishes an unread counter from a status dot at a glance.
 * Renders nothing when the count is zero or less.
 */
function UnreadBadge({
  count,
  max = 99,
  className,
  ...props
}: React.ComponentProps<'span'> & { count: number; max?: number }) {
  if (count <= 0) return null

  return (
    <span
      data-slot="unread-badge"
      className={cn(
        'bg-status-unread-bg text-status-unread-fg inline-flex min-w-4 items-center justify-center px-1 py-px text-[10px] leading-none font-medium tabular-nums',
        className,
      )}
      {...props}
    >
      {count > max ? `${max}+` : count}
    </span>
  )
}

export { StatusDot, UnreadBadge }
export type { Status }
