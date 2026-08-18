import type { ReactNode } from 'react'
import { CheckCircle2, CircleDot, Users, XCircle } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

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
  return (
    <Badge
      title={status === 'clean' ? 'PR merged or closed with zero human interaction.' : status === 'human_in_the_loop' ? 'PR merged or closed; a human interacted via the PR (comment, review, or code push).' : status === 'warning' ? 'PR merged or closed; a human interacted via the factory dashboard.' : status === 'failed' ? 'No PR was ever delivered or the run definitively failed before delivery.' : status === 'running' ? 'In progress; no failure has occurred.' : undefined}
      variant={status === 'failed' ? 'destructive' : status === 'running' ? 'secondary' : 'outline'}
      className={cn(status === 'clean' && 'border-emerald-500/40 text-emerald-700 dark:text-emerald-300', status === 'human_in_the_loop' && 'border-blue-500/50 text-blue-700 dark:text-blue-300', status === 'warning' && 'border-amber-500/50 text-amber-700 dark:text-amber-300')}
    >
      {icon}
      {formatLabel(status)}
    </Badge>
  )
}
