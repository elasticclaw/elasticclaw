import type { AIChatSource } from "@/lib/api"

export function sourceStatus(status: string) {
  switch (status) {
    case "connected": return { label: "Connected", dotClass: "bg-green-500", textClass: "text-muted-foreground", warning: false }
    case "unreachable": return { label: "Can't reach", dotClass: "bg-amber-500", textClass: "text-amber-600 dark:text-amber-400", warning: true }
    case "invalid": return { label: "Invalid", dotClass: "bg-destructive", textClass: "text-destructive", warning: true }
    default: return { label: "Unknown", dotClass: "bg-muted-foreground", textClass: "text-muted-foreground", warning: false }
  }
}

export function sourceSummary(sources: AIChatSource[], loading = false) {
  const connected = sources.filter((source) => source.status === "connected").length
  const allConnected = connected === sources.length
  return {
    label: allConnected ? `${sources.length} sources` : `${connected} of ${sources.length} sources`,
    dotClass: loading || sources.length === 0 ? "bg-muted-foreground" : allConnected ? "bg-green-500" : "bg-amber-500",
  }
}

export function groupSources(sources: AIChatSource[]) {
  const groups = [
    { name: "Knowledge", kinds: ["knowledge_base"] },
    { name: "Repositories", kinds: ["repositories"] },
    { name: "Data", kinds: ["posthog", "datadog"] },
    { name: "Tracker", kinds: ["issue_tracker"] },
  ]
  const knownKinds = groups.flatMap((group) => group.kinds)
  return [
    ...groups.map((group) => ({ name: group.name, sources: sources.filter((source) => group.kinds.includes(source.kind)) })),
    { name: "Other", sources: sources.filter((source) => !knownKinds.includes(source.kind)) },
  ].filter((group) => group.sources.length > 0)
}
