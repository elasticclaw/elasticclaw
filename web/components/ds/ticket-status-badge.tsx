import { CheckCircle2, CircleDot, GitPullRequest, XCircle, type LucideIcon } from 'lucide-react'

import { cn } from '@/lib/utils'

export type TicketStatus = 'delivered' | 'pr_open' | 'in_progress' | 'failed'

export const TICKET_STATUS: Record<TicketStatus, { icon: LucideIcon; label: string; color: string; title: string }> = {
  delivered: { icon: CheckCircle2, label: 'Delivered', color: 'var(--chart-2)', title: 'At least one pull request for this ticket was merged.' },
  pr_open: { icon: GitPullRequest, label: 'PR open', color: 'var(--chart-1)', title: 'Pull requests are open and awaiting review — none merged or closed yet.' },
  in_progress: { icon: CircleDot, label: 'In progress', color: 'var(--chart-5)', title: 'An agent is still working; no pull request has been opened yet.' },
  failed: { icon: XCircle, label: 'Failed', color: 'var(--chart-4)', title: 'Every run on this ticket failed and nothing was delivered.' },
}

export function TicketStatusBadge({ status, size = 12, className }: { status: TicketStatus; size?: number; className?: string }) {
  const state = TICKET_STATUS[status] ?? TICKET_STATUS.in_progress
  const Icon = state.icon
  return <span title={state.title} className={cn('inline-flex w-fit items-center gap-1 rounded-md border px-2 py-0.5 text-xs font-medium whitespace-nowrap', className)} style={{ borderColor: `color-mix(in srgb, ${state.color} 45%, transparent)`, color: state.color }}><Icon size={size} />{state.label}</span>
}
