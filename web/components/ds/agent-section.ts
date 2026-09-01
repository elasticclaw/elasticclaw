import { AlertCircle, Circle, Loader2, type LucideIcon } from 'lucide-react'

import type { Claw, ClawStatus } from '@/lib/types'

export type AgentSectionName = 'attention' | 'working' | 'offline'

export const AGENT_SECTION: Record<
  AgentSectionName,
  { label: string; icon: LucideIcon; color: string; empty: string }
> = {
  attention: {
    label: 'Needs attention',
    icon: AlertCircle,
    color: 'var(--status-idle)',
    empty: 'Nothing waiting on you',
  },
  working: {
    label: 'Working',
    icon: Loader2,
    color: 'var(--status-connected)',
    empty: 'No agents running',
  },
  offline: {
    label: 'Offline',
    icon: Circle,
    color: 'var(--status-offline)',
    empty: 'No stopped agents',
  },
}

type SectionClaw = Pick<Claw, 'status' | 'unreadCount' | 'llm_limited_until'> | {
  status: ClawStatus | 'streaming'
  unreadCount?: number
  llm_limited_until?: string
}

export function agentSection(
  claw: SectionClaw | null | undefined,
  extras: { isWaitingOnYou: boolean },
): AgentSectionName {
  if (!claw || claw.status === 'offline') return 'offline'
  if (claw.status === 'error') return 'attention'
  // A capped provider stops the agent dead. Leaving it under "Working" was the
  // loudest wrong signal on the board — the section a human scans to see what
  // is moving would list an agent that cannot move, and the one thing that
  // could unblock it sooner (raising the account limit) is a human's to do.
  if (claw.llm_limited_until) return 'attention'
  if (claw.status === 'idle') {
    return claw.unreadCount && claw.unreadCount > 0 || extras.isWaitingOnYou
      ? 'attention'
      : 'offline'
  }
  return 'working'
}
