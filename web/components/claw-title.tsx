"use client"

import { cn } from "@/lib/utils"

export function IssueAwareTitle({ name }: { name: string }) {
  const lastSlash = name.lastIndexOf("/")
  if (lastSlash < 0 || lastSlash === name.length - 1) {
    return <span className="block min-w-0 truncate">{name}</span>
  }

  return (
    <span className="min-w-0 flex items-baseline">
      <span className="min-w-0 truncate">{name.slice(0, lastSlash + 1)}</span>
      <span className="shrink-0">{name.slice(lastSlash + 1)}</span>
    </span>
  )
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
