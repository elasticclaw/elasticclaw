"use client"

import { cn } from "@/lib/utils"

export function IssueAwareTitle({ name }: { name: string }) {
  const lastSlash = name.lastIndexOf("/")
  if (lastSlash <= 0 || lastSlash === name.length - 1) {
    return <span className="block min-w-0 truncate">{name}</span>
  }

  // The path segment gives way first (high shrink factor); the issue segment
  // only starts truncating once the path is fully collapsed. Neither segment
  // is shrink-proof: a hard shrink-0 tail wider than the container would
  // overflow and paint under whatever sits next to the title (the status
  // badge, on narrow phone headers).
  return (
    <span className="min-w-0 max-w-full flex items-baseline">
      <span className="min-w-0 shrink-[100] truncate">{name.slice(0, lastSlash + 1)}</span>
      <span className="min-w-0 truncate">{name.slice(lastSlash + 1)}</span>
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
