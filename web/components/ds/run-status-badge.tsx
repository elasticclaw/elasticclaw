import type { ReactNode } from 'react'
import { CheckCircle2, CircleDot, Users, XCircle } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
function formatLabel(value: string) {
  return value.replace(/_/g, ' ').replace(/\b\w/g, (match) => match.toUpperCase())
}

export function RunStatusBadge({ status }: { status: string }) {
  const statusIcons: Record<string, ReactNode> = {
    clean: <CheckCircle2 className="size-3" />,
    human_in_the_loop: <Users className="size-3" />,
    failed: <XCircle className="size-3" />,
  }
  const icon = statusIcons[status] ?? <CircleDot className="size-3" />
  const color = status === 'clean' ? 'var(--chart-2)' : status === 'human_in_the_loop' ? 'var(--chart-1)' : status === 'warning' ? 'var(--text-warning)' : undefined
  return (
    <Badge
      title={status === 'clean' ? 'PR merged or closed with zero human interaction.' : status === 'human_in_the_loop' ? 'PR merged or closed; a human interacted via the PR (comment, review, or code push).' : status === 'warning' ? 'PR merged or closed; a human interacted via the factory dashboard.' : status === 'failed' ? 'No PR was ever delivered or the run definitively failed before delivery.' : status === 'running' ? 'In progress; no failure has occurred.' : undefined}
      variant={status === 'failed' ? 'destructive' : status === 'running' ? 'secondary' : 'outline'}
      style={color ? { borderColor: `color-mix(in srgb, ${color} 45%, transparent)`, color } : undefined}
    >
      {icon}
      {formatLabel(status)}
    </Badge>
  )
}
