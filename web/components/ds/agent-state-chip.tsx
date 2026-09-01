import { cva, type VariantProps } from 'class-variance-authority'
import type * as React from 'react'

import type { ClawStatus } from '@/lib/types'
import { cn } from '@/lib/utils'

export type AgentStateStatus = ClawStatus | 'streaming' | 'paused'

const AGENT_STATE = {
  streaming: { label: 'RUNNING', color: 'var(--status-streaming)' },
  connected: { label: 'READY', color: 'var(--status-connected)' },
  // A claw whose provider has no allowance left is connected, healthy, and
  // going nowhere. READY was the worst thing the row could say about it: the
  // socket is up, so every other signal agreed with READY while nothing ran.
  // Amber matches the limit chip that supplies the reason beside it.
  paused: { label: 'PAUSED', color: 'var(--status-idle)' },
  provisioning: { label: 'STARTING', color: 'var(--status-provisioning)' },
  idle: { label: 'IDLE', color: 'var(--status-idle)' },
  offline: { label: 'OFFLINE', color: 'var(--status-offline)' },
  error: { label: 'ERROR', color: 'var(--status-error)' },
} as const

const agentStateChipVariants = cva(
  'inline-flex w-fit shrink-0 items-center gap-1 rounded-sm font-mono font-medium leading-4 tracking-[0.04em]',
  {
    variants: {
      size: {
        sm: 'px-1 text-[10px]',
        md: 'px-1.5 py-px text-[11px]',
      },
    },
    defaultVariants: { size: 'sm' },
  },
)

export interface AgentStateChipProps
  extends React.ComponentProps<'span'>,
    VariantProps<typeof agentStateChipVariants> {
  status: AgentStateStatus
  isStreaming?: boolean
  reason?: string
}

export function AgentStateChip({
  status,
  isStreaming = false,
  size,
  reason,
  className,
  style,
  ...props
}: AgentStateChipProps) {
  const state = AGENT_STATE[isStreaming ? 'streaming' : status] ?? AGENT_STATE.connected
  const muted = status === 'offline' && !isStreaming

  return (
    <span
      className={cn(agentStateChipVariants({ size }), className)}
      style={{
        backgroundColor: muted
          ? 'var(--muted)'
          : `color-mix(in srgb, ${state.color} 16%, transparent)`,
        color: state.color,
        ...style,
      }}
      title={reason}
      {...props}
    >
      {state.label}
      {reason ? <span className="opacity-75 tracking-normal">· {reason}</span> : null}
    </span>
  )
}
