"use client"

import { cn } from "@/lib/utils"

export function IssueAwareTitle({ name }: { name: string }) {
  // One plain truncating span, like the kit's AgentRow: the two-segment split
  // truncated both halves early and left free space to the right of the name.
  return <span className="block min-w-0 truncate">{name}</span>
}

export function ClawTitle({
  name,
  githubIssueId,
  githubIssueUrl,
  className,
}: {
  name: string
  githubIssueId?: string
  githubIssueUrl?: string
  className?: string
}) {
  if (!githubIssueUrl) {
    return (
      <span className={cn("min-w-0", className)}>
        <IssueAwareTitle name={name} />
      </span>
    )
  }

  return (
    <a
      href={githubIssueUrl}
      target="_blank"
      rel="noopener noreferrer"
      title={githubIssueId ? `Open ${githubIssueId}` : "Open GitHub issue"}
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => e.stopPropagation()}
      className={cn("min-w-0 rounded-sm hover:underline focus:outline-none focus:ring-1 focus:ring-ring", className)}
    >
      <IssueAwareTitle name={name} />
    </a>
  )
}
